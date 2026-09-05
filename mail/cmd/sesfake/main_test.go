package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/internal/sesfake"
)

// sendEmailBody — тело SendEmail с одним получателем.
const sendEmailBody = `{"FromEmailAddress":"noreply@example.ru",` +
	`"Destination":{"ToAddresses":["teacher@school.ru"]},` +
	`"ReplyToAddresses":["support@example.ru"],` +
	`"Content":{"Simple":{"Subject":{"Data":"Тема"},"Body":{"Text":{"Data":"Текст"}},` +
	`"Headers":[{"Name":"X-Trace","Value":"trace-42"}]}}}`

// formValidAuthorization — заголовок правильной формы: без -secret обработчик
// подпись не пересчитывает. %s — дата YYYYMMDD.
const formValidAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/%s/ru-central1/ses/aws4_request, " +
	"SignedHeaders=content-type;host;x-amz-date, " +
	"Signature=0000000000000000000000000000000000000000000000000000000000000000"

func request(t *testing.T, method, url, body string) (status int, raw string) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("Authorization", strings.Replace(formValidAuthorization, "%s", now.Format("20060102"), 1))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	rawBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(rawBytes)
}

func TestMux_StoreHealthzAndSESRoutes(t *testing.T) {
	t.Parallel()
	handler := sesfake.NewHandler()
	srv := httptest.NewServer(newMux(handler))
	t.Cleanup(srv.Close)

	status, _ := request(t, http.MethodGet, srv.URL+"/healthz", "")
	assert.Equal(t, http.StatusOK, status)

	status, raw := request(t, http.MethodGet, srv.URL+"/store", "")
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `[]`, raw, "пустое хранилище — массив, а не null")

	status, _ = request(t, http.MethodPost, srv.URL+sesfake.SendEmailPath, sendEmailBody)
	require.Equal(t, http.StatusOK, status)

	status, raw = request(t, http.MethodGet, srv.URL+"/store", "")
	require.Equal(t, http.StatusOK, status)
	var stored []sesfake.SentEmail
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.Len(t, stored, 1)
	assert.Equal(t, "teacher@school.ru", stored[0].To)
	assert.Equal(t, "Тема", stored[0].Subject)
	assert.Equal(t, []string{"support@example.ru"}, stored[0].ReplyTo)
	assert.NotEmpty(t, stored[0].MessageID)

	status, _ = request(t, http.MethodDelete, srv.URL+"/store", "")
	assert.Equal(t, http.StatusNoContent, status)
	status, raw = request(t, http.MethodGet, srv.URL+"/store", "")
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `[]`, raw)

	// Неизвестный маршрут отвечает как провайдер, а не текстовой 404 mux.
	status, raw = request(t, http.MethodGet, srv.URL+"/v2/email/configuration-sets", "")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, raw, `"Code":"UnknownOperationException"`)
}

// Релей включён — принятое письмо доходит до SMTP стенда; ответ клиенту не ждёт
// доставки, поэтому тест ждёт релей сам.
func TestMux_RelaysAcceptedEmail(t *testing.T) {
	t.Parallel()
	smtpSrv := startFakeSMTP(t)
	relay := newRelayer(smtpSrv.addr, newDiscardLogger())
	handler := sesfake.NewHandler()
	handler.OnAccepted = relay.enqueue
	srv := httptest.NewServer(newMux(handler))
	t.Cleanup(srv.Close)

	status, _ := request(t, http.MethodPost, srv.URL+sesfake.SendEmailPath, sendEmailBody)
	require.Equal(t, http.StatusOK, status)
	relay.wait()

	d := <-smtpSrv.deliveries
	assert.Equal(t, []string{"EHLO", "MAIL", "RCPT", "DATA", "QUIT"}, d.commands)
	assert.Contains(t, d.message, "X-Trace: trace-42")
	assert.Contains(t, d.message, "Reply-To: <support@example.ru>")
}

func TestParseFlags_RejectRepeatsAndFormat(t *testing.T) {
	t.Parallel()
	opts, err := parseFlags([]string{"-reject", "A@b.ru=MessageRejected", "-reject", "c@d.ru=BadRequestException"}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, rejectFlag{"a@b.ru": "MessageRejected", "c@d.ru": "BadRequestException"}, opts.reject)
	assert.Equal(t, "a@b.ru=MessageRejected,c@d.ru=BadRequestException", opts.reject.String())

	for _, bad := range []string{"no-equals", "=Code", "a@b.ru=", ""} {
		_, err = parseFlags([]string{"-reject", bad}, io.Discard)
		assert.Error(t, err, "%q должно быть отвергнуто", bad)
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	t.Parallel()
	opts, err := parseFlags(nil, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, ":8080", opts.listen)
	assert.Equal(t, 1000, opts.storeLimit)
	assert.Empty(t, opts.secret)
	assert.Empty(t, opts.relay)
	assert.Empty(t, opts.reject)

	_, err = parseFlags([]string{"-store-limit", "-1"}, io.Discard)
	assert.Error(t, err)
}

// Окружение задаёт значения по умолчанию, флаг сильнее (docker-compose против
// отладки руками). t.Setenv несовместим с t.Parallel — тест последовательный.
func TestParseFlags_EnvironmentDefaults(t *testing.T) {
	t.Setenv("SESFAKE_LISTEN", ":9000")
	t.Setenv("SESFAKE_SECRET", "stand-secret")
	t.Setenv("SESFAKE_REGION", "ru-central1")
	t.Setenv("SESFAKE_RELAY", "mailpit:1025")
	t.Setenv("SESFAKE_STORE_LIMIT", "10")
	t.Setenv("SESFAKE_REJECT", "a@b.ru=MessageRejected,c@d.ru=AccountSuspendedException")

	opts, err := parseFlags(nil, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, ":9000", opts.listen)
	assert.Equal(t, "stand-secret", opts.secret)
	assert.Equal(t, "ru-central1", opts.region)
	assert.Equal(t, "mailpit:1025", opts.relay)
	assert.Equal(t, 10, opts.storeLimit)
	assert.Equal(t, rejectFlag{"a@b.ru": "MessageRejected", "c@d.ru": "AccountSuspendedException"}, opts.reject)

	opts, err = parseFlags([]string{"-listen", ":7000", "-reject", "e@f.ru=MessageRejected"}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, ":7000", opts.listen)
	assert.Equal(t, rejectFlag{"e@f.ru": "MessageRejected"}, opts.reject, "флаг заменяет список из окружения")

	t.Setenv("SESFAKE_STORE_LIMIT", "много")
	_, err = parseFlags(nil, io.Discard)
	assert.Error(t, err)
}

func TestSecretState_NeverEchoesSecret(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, secretState("stand-secret"), "stand-secret")
	assert.Contains(t, secretState(""), "form")
}
