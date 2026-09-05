package mailtest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
	"github.com/nrect/rebar/mail/sesv2"
)

const sendPath = "/v2/email/outbound-emails"

// formValidAuthorization — заголовок правильной ФОРМЫ с произвольной
// подписью: при пустом Secret двойник её не пересчитывает, и так тесты
// двойника обходятся без собственного подписанта. %s — дата YYYYMMDD.
const formValidAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/%s/ru-central1/ses/aws4_request, " +
	"SignedHeaders=content-type;host;x-amz-date, Signature=0000000000000000000000000000000000000000000000000000000000000000"

func simpleBody(to string, headers map[string]string) string {
	type nv struct{ Name, Value string }
	hdrs := make([]nv, 0, len(headers))
	for k, v := range headers {
		hdrs = append(hdrs, nv{k, v})
	}
	raw, err := json.Marshal(map[string]any{
		"FromEmailAddress": "noreply@example.ru",
		"Destination":      map[string]any{"ToAddresses": []string{to}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]string{"Data": "Тема", "Charset": "UTF-8"},
			"Body":    map[string]any{"Text": map[string]string{"Data": "Текст", "Charset": "UTF-8"}},
			"Headers": hdrs,
		}},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// rawPost шлёт запрос с заголовками как есть; Authorization и X-Amz-Date
// добавляются, если их нет в headers (пустое значение — не добавлять).
func rawPost(t *testing.T, url, body string, headers map[string]string) (status int, code, raw string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+sendPath, strings.NewReader(body))
	require.NoError(t, err)
	now := time.Now().UTC()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("Authorization", strings.Replace(formValidAuthorization, "%s", now.Format("20060102"), 1))
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	rawBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("X-Amzn-ErrorType"), string(rawBytes)
}

func TestSESServer_RequiresSignatureForm(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	body := simpleBody("teacher@school.ru", nil)
	today := time.Now().UTC().Format("20060102")

	cases := []struct {
		name     string
		headers  map[string]string
		wantCode string
	}{
		{name: "no Authorization", headers: map[string]string{"Authorization": ""}, wantCode: "MissingAuthenticationTokenException"},
		{name: "bearer token", headers: map[string]string{"Authorization": "Bearer x"}, wantCode: "IncompleteSignatureException"},
		{name: "no X-Amz-Date", headers: map[string]string{"X-Amz-Date": ""}, wantCode: "IncompleteSignatureException"},
		{
			name: "wrong service", wantCode: "InvalidSignatureException",
			headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + today +
				"/ru-central1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab"},
		},
		{
			name: "missing x-amz-date in SignedHeaders", wantCode: "IncompleteSignatureException",
			headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + today +
				"/ru-central1/ses/aws4_request, SignedHeaders=host, Signature=ab"},
		},
		{
			name: "unsorted SignedHeaders", wantCode: "IncompleteSignatureException",
			headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + today +
				"/ru-central1/ses/aws4_request, SignedHeaders=x-amz-date;host, Signature=ab"},
		},
		{
			name: "credential date differs from X-Amz-Date", wantCode: "InvalidSignatureException",
			headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830" +
				"/ru-central1/ses/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code, raw := rawPost(t, srv.URL(), body, tc.headers)
			assert.Equal(t, http.StatusForbidden, status, raw)
			assert.Equal(t, tc.wantCode, code)
			assert.Contains(t, raw, `"Code":"`+tc.wantCode+`"`, "тело в формате Postbox")
		})
	}
}

func TestSESServer_ChecksRegionWhenSet(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	srv.Region = "us-east-1"
	status, code, _ := rawPost(t, srv.URL(), simpleBody("teacher@school.ru", nil), nil)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "InvalidSignatureException", code)
	assert.Empty(t, srv.Sent())
}

// Двойник ведёт себя как провайдер: заголовки из запретного списка — 400
// BadRequestException, второй получатель — тоже. Именно так адаптер узнал бы
// о своей ошибке на проде.
func TestSESServer_RejectsRestrictedHeadersAndExtraRecipients(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)

	for _, name := range []string{"Message-ID", "message-id", "Reply-To", "From", "To", "Subject", "Date", "Content-Type"} {
		status, code, raw := rawPost(t, srv.URL(), simpleBody("teacher@school.ru", map[string]string{name: "x"}), nil)
		assert.Equal(t, http.StatusBadRequest, status, "%s: %s", name, raw)
		assert.Equal(t, "BadRequestException", code, name)
	}
	status, code, _ := rawPost(t, srv.URL(), simpleBody("teacher@school.ru", map[string]string{"X-Trace": "ok"}), nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, code)

	twoRecipients := strings.Replace(simpleBody("a@example.ru", nil), `["a@example.ru"]`, `["a@example.ru","b@example.ru"]`, 1)
	status, code, _ = rawPost(t, srv.URL(), twoRecipients, nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "BadRequestException", code)

	status, code, _ = rawPost(t, srv.URL(), `{"FromEmailAddress":"a@b.ru","Destination":{"ToAddresses":["c@d.ru"]},"Content":{"Raw":{"Data":"x"}}}`, nil)
	assert.Equal(t, http.StatusBadRequest, status, "Raw не поддерживается")
	assert.Equal(t, "BadRequestException", code)

	status, code, _ = rawPost(t, srv.URL(), `not json`, nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "BadRequestException", code)

	require.Len(t, srv.Sent(), 1)
	assert.Equal(t, map[string]string{"X-Trace": "ok"}, srv.Sent()[0].Headers)
}

func TestSESServer_UnknownRouteIs404(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL()+"/v2/email/configuration-sets", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "UnknownOperationException", resp.Header.Get("X-Amzn-ErrorType"))
}

// RejectFor и ThrottleFor ключуются по email в нижнем регистре, даже если
// адрес пришёл с именем и в другом регистре.
func TestSESServer_RejectAndThrottleKeyByBareEmail(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	srv.RejectFor["teacher@school.ru"] = "MailFromDomainNotVerifiedException"
	srv.ThrottleFor["other@school.ru"] = 2

	status, code, raw := rawPost(t, srv.URL(), simpleBody("Учитель <Teacher@School.RU>", nil), nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "MailFromDomainNotVerifiedException", code)
	assert.Contains(t, raw, `"message":"rejected by mailtest.SESServer for teacher@school.ru"`)

	for range 2 {
		status, code, _ = rawPost(t, srv.URL(), simpleBody("OTHER@school.ru", nil), nil)
		assert.Equal(t, http.StatusTooManyRequests, status)
		assert.Equal(t, "TooManyRequestsException", code)
	}
	status, _, raw = rawPost(t, srv.URL(), simpleBody("other@school.ru", nil), nil)
	assert.Equal(t, http.StatusOK, status)
	var ok struct {
		MessageID string `json:"MessageId"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &ok))
	assert.NotEmpty(t, ok.MessageID)

	sent := srv.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, ok.MessageID, sent[0].MessageID)
	assert.Equal(t, "other@school.ru", sent[0].To)
	assert.Equal(t, "noreply@example.ru", sent[0].From)
	assert.Equal(t, "Тема", sent[0].Subject)
	assert.Equal(t, "Текст", sent[0].Text)
}

// Полная проверка подписи по секрету — через настоящий адаптер: правильный
// секрет проходит, неправильный — 403 InvalidSignatureException.
func TestSESServer_VerifiesSignatureWithSecret(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	srv.Secret = "correct-secret"
	env := mail.Envelope{
		To: mail.Address{Email: "teacher@school.ru"}, From: mail.Address{Email: "noreply@example.ru"},
		Subject: "s", Text: "t",
	}

	good, err := sesv2.New(sesv2.Config{Endpoint: srv.URL(), Region: "ru-central1", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "correct-secret"})
	require.NoError(t, err)
	_, err = good.Send(t.Context(), env)
	require.NoError(t, err)

	bad, err := sesv2.New(sesv2.Config{Endpoint: srv.URL(), Region: "ru-central1", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wrong-secret"})
	require.NoError(t, err)
	_, err = bad.Send(t.Context(), env)
	require.Error(t, err)
	var perr *sesv2.ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, http.StatusForbidden, perr.Status)
	assert.Equal(t, "InvalidSignatureException", perr.Code)
	assert.Len(t, srv.Sent(), 1)
}

// Двойник потокобезопасен: параллельные отправки не теряются и не гонятся
// (тест идёт под -race). Внутри горутин — без require: FailNow из чужой
// горутины не останавливает тест, а лишь роняет её.
func TestSESServer_ConcurrentSends(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	const n = 25
	var wg sync.WaitGroup
	statuses := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], errs[i] = postStatus(t.Context(), srv.URL(), simpleBody("teacher@school.ru", nil))
		}()
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, http.StatusOK, statuses[i])
	}
	sent := srv.Sent()
	assert.Len(t, sent, n)
	ids := make(map[string]bool, n)
	for _, e := range sent {
		ids[e.MessageID] = true
	}
	assert.Len(t, ids, n, "MessageId уникален")
}

// postStatus — rawPost без утверждений: для вызова из горутин.
func postStatus(ctx context.Context, url, body string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+sendPath, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("Authorization", strings.Replace(formValidAuthorization, "%s", now.Format("20060102"), 1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
