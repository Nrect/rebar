package mailtest_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/mailtest"
)

// Обработчик SES v2 и его поведение проверяются в internal/sesfake, а сквозной
// путь «адаптер → двойник» — в sesv2_test.go. Здесь только то, что добавляет
// обёртка: живой httptest, проброс полей и методов, своё имя в отказах.

const sendPath = "/v2/email/outbound-emails"

// formValidAuthorization — заголовок правильной ФОРМЫ с произвольной подписью:
// при пустом Secret двойник её не пересчитывает. %s — дата YYYYMMDD.
const formValidAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/%s/ru-central1/ses/aws4_request, " +
	"SignedHeaders=content-type;host;x-amz-date, " +
	"Signature=0000000000000000000000000000000000000000000000000000000000000000"

const sendEmailBody = `{"FromEmailAddress":"noreply@example.ru",` +
	`"Destination":{"ToAddresses":["Учитель <Teacher@School.RU>"]},` +
	`"Content":{"Simple":{"Subject":{"Data":"Тема"},"Body":{"Text":{"Data":"Текст"}}}}}`

func post(t *testing.T, url string) (status int, code, raw string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+sendPath, strings.NewReader(sendEmailBody))
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
	return resp.StatusCode, resp.Header.Get("X-Amzn-ErrorType"), string(rawBytes)
}

// URL() ведёт на живой сервер, а Sent/Reset достаются из встроенного обработчика.
func TestSESServer_ServesHandlerOverHTTP(t *testing.T) {
	t.Parallel()
	srv := mailtest.NewSESServer(t)
	require.True(t, strings.HasPrefix(srv.URL(), "http://"), srv.URL())

	status, _, raw := post(t, srv.URL())
	require.Equal(t, http.StatusOK, status, raw)

	sent := srv.Sent() // []mailtest.SentEmail — псевдоним типа обработчика
	require.Len(t, sent, 1)
	got := sent[0]
	assert.Equal(t, "Учитель <Teacher@School.RU>", got.To)
	assert.Equal(t, "Тема", got.Subject)
	assert.Equal(t, "Текст", got.Text)
	assert.Contains(t, raw, got.MessageID)

	srv.Reset()
	assert.Empty(t, srv.Sent())
}

// Поля-настройки доходят до обработчика через встраивание, а имя двойника в
// отказах — своё: на текст «mailtest.SESServer» смотрит sesv2_test.go.
func TestSESServer_ForwardsSettingsAndNamesItself(t *testing.T) {
	t.Parallel()

	rejecting := mailtest.NewSESServer(t)
	rejecting.RejectFor["teacher@school.ru"] = "MessageRejected"
	status, code, raw := post(t, rejecting.URL())
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "MessageRejected", code)
	assert.Contains(t, raw, `"message":"rejected by mailtest.SESServer for teacher@school.ru"`)
	assert.Empty(t, rejecting.Sent())

	regional := mailtest.NewSESServer(t)
	regional.Region = "us-east-1"
	status, code, _ = post(t, regional.URL())
	assert.Equal(t, http.StatusForbidden, status, "Credential в ru-central1 при Region=us-east-1")
	assert.Equal(t, "InvalidSignatureException", code)
	assert.Empty(t, regional.Sent())
}
