package sesfake

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// formValidAuthorization — заголовок правильной ФОРМЫ с произвольной подписью:
// при пустом Secret обработчик её не пересчитывает. %s — дата YYYYMMDD.
const formValidAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/%s/ru-central1/ses/aws4_request, " +
	"SignedHeaders=content-type;host;x-amz-date, " +
	"Signature=0000000000000000000000000000000000000000000000000000000000000000"

func newServer(t *testing.T) (handler *Handler, url string) {
	t.Helper()
	h := NewHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

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

// newRequest — POST на SendEmailPath с формально годными Authorization и
// X-Amz-Date; secret непуст — подпись считается по-настоящему.
func newRequest(ctx context.Context, url, body, secret string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+SendEmailPath, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	amzDate := now.Format(amzDateLayout)
	req.Host = req.URL.Host // подписывается host, который увидит сервер
	req.Header.Set("Content-Type", contentTypeJSON)
	req.Header.Set(amzDateHeader, amzDate)
	req.Header.Set("Authorization", strings.Replace(formValidAuthorization, "%s", now.Format("20060102"), 1))
	if secret == "" {
		return req, nil
	}
	auth := authorization{
		accessKeyID:   "AKIDEXAMPLE",
		date:          now.Format("20060102"),
		region:        "ru-central1",
		service:       sigService,
		signedHeaders: []string{"content-type", hostHeader, amzDateHeader},
	}
	req.Header.Set("Authorization", sigAlgorithm+" Credential="+auth.accessKeyID+"/"+auth.date+"/"+auth.region+
		"/"+auth.service+"/"+sigTerminator+", SignedHeaders="+strings.Join(auth.signedHeaders, ";")+
		", Signature="+expectedSignature(req, []byte(body), auth, amzDate, secret))
	return req, nil
}

// post шлёт запрос, подменяя заголовки из headers (пустое значение — удалить).
func post(t *testing.T, url, body, secret string, headers map[string]string) (status int, code, raw string) {
	t.Helper()
	req, err := newRequest(t.Context(), url, body, secret)
	require.NoError(t, err)
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
	return resp.StatusCode, resp.Header.Get(headerAmzErrorType), string(rawBytes)
}

// messageID — MessageId из успешного ответа.
func messageID(t *testing.T, raw string) string {
	t.Helper()
	var ok struct {
		MessageID string `json:"MessageId"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &ok))
	require.NotEmpty(t, ok.MessageID)
	return ok.MessageID
}

func TestHandler_RequiresSignatureForm(t *testing.T) {
	t.Parallel()
	_, url := newServer(t)
	body := simpleBody("teacher@school.ru", nil)
	today := time.Now().UTC().Format("20060102")
	credential := func(region, service, signedHeaders string) map[string]string {
		return map[string]string{"Authorization": sigAlgorithm + " Credential=AKIDEXAMPLE/" + today +
			"/" + region + "/" + service + "/aws4_request, SignedHeaders=" + signedHeaders + ", Signature=ab"}
	}

	cases := []struct {
		name     string
		headers  map[string]string
		wantCode string
	}{
		{name: "no Authorization", headers: map[string]string{"Authorization": ""}, wantCode: codeMissingAuth},
		{name: "bearer token", headers: map[string]string{"Authorization": "Bearer x"}, wantCode: codeIncompleteSig},
		{name: "no X-Amz-Date", headers: map[string]string{amzDateHeader: ""}, wantCode: codeIncompleteSig},
		{name: "wrong service", headers: credential("ru-central1", "s3", "host;x-amz-date"), wantCode: codeInvalidSignature},
		{name: "missing x-amz-date in SignedHeaders", headers: credential("ru-central1", "ses", "host"), wantCode: codeIncompleteSig},
		{name: "unsorted SignedHeaders", headers: credential("ru-central1", "ses", "x-amz-date;host"), wantCode: codeIncompleteSig},
		{
			name: "credential date differs from X-Amz-Date", wantCode: codeInvalidSignature,
			headers: map[string]string{"Authorization": sigAlgorithm + " Credential=AKIDEXAMPLE/20150830" +
				"/ru-central1/ses/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code, raw := post(t, url, body, "", tc.headers)
			assert.Equal(t, http.StatusForbidden, status, raw)
			assert.Equal(t, tc.wantCode, code)
			assert.Contains(t, raw, `"Code":"`+tc.wantCode+`"`, "тело в формате Postbox")
		})
	}
}

func TestHandler_ChecksRegionWhenSet(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	h.Region = "us-east-1"
	status, code, _ := post(t, url, simpleBody("teacher@school.ru", nil), "", nil)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, codeInvalidSignature, code)
	assert.Empty(t, h.Sent())
}

// Полная проверка подписи по Secret: правильный секрет проходит, неправильный —
// 403 InvalidSignatureException (тот же вердикт, что даёт настоящий SES).
func TestHandler_VerifiesSignatureWithSecret(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	h.Secret = "correct-secret"
	body := simpleBody("teacher@school.ru", nil)

	status, _, raw := post(t, url, body, "correct-secret", nil)
	require.Equal(t, http.StatusOK, status, raw)

	status, code, _ := post(t, url, body, "wrong-secret", nil)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, codeInvalidSignature, code)
	assert.Len(t, h.Sent(), 1)
}

// Обработчик ведёт себя как провайдер: заголовки из запретного списка — 400
// BadRequestException, второй получатель — тоже.
func TestHandler_RejectsRestrictedHeadersAndExtraRecipients(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)

	for _, name := range []string{"Message-ID", "message-id", "Reply-To", "From", "To", "Subject", "Date", "Content-Type"} {
		status, code, raw := post(t, url, simpleBody("teacher@school.ru", map[string]string{name: "x"}), "", nil)
		assert.Equal(t, http.StatusBadRequest, status, "%s: %s", name, raw)
		assert.Equal(t, codeBadRequest, code, name)
	}
	status, code, _ := post(t, url, simpleBody("teacher@school.ru", map[string]string{"X-Trace": "ok"}), "", nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, code)

	for _, bad := range []string{
		strings.Replace(simpleBody("a@example.ru", nil), `["a@example.ru"]`, `["a@example.ru","b@example.ru"]`, 1),
		`{"FromEmailAddress":"a@b.ru","Destination":{"ToAddresses":["c@d.ru"]},"Content":{"Raw":{"Data":"x"}}}`,
		`not json`,
	} {
		status, code, _ = post(t, url, bad, "", nil)
		assert.Equal(t, http.StatusBadRequest, status, bad)
		assert.Equal(t, codeBadRequest, code, bad)
	}

	require.Len(t, h.Sent(), 1)
	assert.Equal(t, map[string]string{"X-Trace": "ok"}, h.Sent()[0].Headers)
}

func TestHandler_UnknownRouteIs404(t *testing.T) {
	t.Parallel()
	_, url := newServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/v2/email/configuration-sets", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, codeUnknownOperation, resp.Header.Get(headerAmzErrorType))
}

// RejectFor и ThrottleFor ключуются по email в нижнем регистре, даже если адрес
// пришёл с именем и в другом регистре; Name попадает в текст ошибки.
func TestHandler_RejectAndThrottleKeyByBareEmail(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	h.RejectFor["teacher@school.ru"] = "MailFromDomainNotVerifiedException"
	h.ThrottleFor["other@school.ru"] = 2

	status, code, raw := post(t, url, simpleBody("Учитель <Teacher@School.RU>", nil), "", nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "MailFromDomainNotVerifiedException", code)
	assert.Contains(t, raw, `"message":"rejected by sesfake for teacher@school.ru"`)

	for range 2 {
		status, code, _ = post(t, url, simpleBody("OTHER@school.ru", nil), "", nil)
		assert.Equal(t, http.StatusTooManyRequests, status)
		assert.Equal(t, codeTooManyRequests, code)
	}
	status, _, raw = post(t, url, simpleBody("other@school.ru", nil), "", nil)
	require.Equal(t, http.StatusOK, status)

	sent := h.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, messageID(t, raw), sent[0].MessageID)
	assert.Equal(t, "other@school.ru", sent[0].To)
	assert.Equal(t, "noreply@example.ru", sent[0].From)
	assert.Equal(t, "Тема", sent[0].Subject)
	assert.Equal(t, "Текст", sent[0].Text)
}

// StoreLimit держит последние N: старые письма вытесняются, порядок сохраняется.
func TestHandler_StoreLimitKeepsLast(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	h.StoreLimit = 3

	ids := make([]string, 0, 5)
	for range 5 {
		status, _, raw := post(t, url, simpleBody("teacher@school.ru", nil), "", nil)
		require.Equal(t, http.StatusOK, status)
		ids = append(ids, messageID(t, raw))
	}
	sent := h.Sent()
	require.Len(t, sent, 3)
	for i, e := range sent {
		assert.Equal(t, ids[i+2], e.MessageID)
	}
}

func TestHandler_ResetClearsStore(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	status, _, _ := post(t, url, simpleBody("teacher@school.ru", nil), "", nil)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, h.Sent(), 1)

	h.Reset()
	assert.Empty(t, h.Sent())

	status, _, _ = post(t, url, simpleBody("teacher@school.ru", nil), "", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, h.Sent(), 1, "после Reset приём продолжается")
}

// Обработчик потокобезопасен: параллельные отправки не теряются и не гонятся
// (тест идёт под -race). Внутри горутин — без require: FailNow из чужой горутины
// не останавливает тест, а лишь роняет её.
func TestHandler_ConcurrentSends(t *testing.T) {
	t.Parallel()
	h, url := newServer(t)
	const n = 25
	var wg sync.WaitGroup
	statuses := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], errs[i] = postStatus(t.Context(), url, simpleBody("teacher@school.ru", nil))
		}()
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, http.StatusOK, statuses[i])
	}
	sent := h.Sent()
	assert.Len(t, sent, n)
	ids := make(map[string]bool, n)
	for _, e := range sent {
		ids[e.MessageID] = true
	}
	assert.Len(t, ids, n, "MessageId уникален")
}

// postStatus — post без утверждений: для вызова из горутин.
func postStatus(ctx context.Context, url, body string) (int, error) {
	req, err := newRequest(ctx, url, body, "")
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
