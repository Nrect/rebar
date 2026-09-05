package sesv2_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
	"github.com/nrect/rebar/mail/sesv2"
)

const (
	testRegion = "ru-central1"
	testKeyID  = "YCAJEexampleKeyId0000000"
	testSecret = "YCexampleSecretKey000000000000000000000"
	// secretToken — то, что не должно утечь ни в одну ошибку: ссылка в теле.
	secretToken = "SECRET-TOKEN-4f9c"
)

func newTransport(t *testing.T, endpoint string, mutate func(*sesv2.Config)) *sesv2.Transport {
	t.Helper()
	cfg := sesv2.Config{
		Endpoint:         endpoint,
		Region:           testRegion,
		AccessKeyID:      testKeyID,
		SecretAccessKey:  testSecret,
		ConfigurationSet: "prod",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	tr, err := sesv2.New(cfg)
	require.NoError(t, err)
	return tr
}

func newServer(t *testing.T) *mailtest.SESServer {
	t.Helper()
	srv := mailtest.NewSESServer(t)
	srv.Secret = testSecret
	srv.Region = testRegion
	return srv
}

func envelope() mail.Envelope {
	return mail.Envelope{
		ID:      uuid.MustParse("6f1d2c3b-4a5e-4f60-9b71-8c2d3e4f5a6b"),
		Kind:    "verify",
		To:      mail.Address{Email: "teacher@school.ru", Name: "Учитель"},
		From:    mail.Address{Email: "noreply@example.ru", Name: "Пример"},
		Subject: "Подтверждение почты",
		Text:    "Ссылка: https://example.ru/verify?token=" + secretToken,
		HTML:    `<a href="https://example.ru/verify?token=` + secretToken + `">Подтвердить</a>`,
		Headers: map[string]string{
			"Reply-To":         "support@example.ru",
			"X-Trace":          "t-1",
			"List-Unsubscribe": "<mailto:unsub@example.ru>",
		},
		MessageID: "<6f1d2c3b-4a5e-4f60-9b71-8c2d3e4f5a6b@example.ru>",
	}
}

func TestName(t *testing.T) {
	t.Parallel()
	tr := newTransport(t, "https://postbox.cloud.yandex.net", nil)
	assert.Equal(t, sesv2.Name, tr.Name())
	assert.Equal(t, mail.TransportName("sesv2"), tr.Name())
}

// Успешная отправка через двойник с полной проверкой подписи по секрету и
// всех полей тела. Отдельно проверяется, что запрещённые провайдером
// заголовки не уходят в Headers[]: Message-ID не передаётся вовсе, Reply-To
// становится ReplyToAddresses.
func TestSend_Success(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), nil)

	res, err := tr.Send(t.Context(), envelope())
	require.NoError(t, err)

	sent := srv.Sent()
	require.Len(t, sent, 1)
	got := sent[0]
	assert.NotEmpty(t, res.ProviderMessageID)
	assert.Equal(t, got.MessageID, res.ProviderMessageID, "ProviderMessageID — то, что назвал провайдер")

	wantFrom := (&netmail.Address{Name: "Пример", Address: "noreply@example.ru"}).String()
	assert.Equal(t, wantFrom, got.From)
	assert.True(t, strings.HasPrefix(got.From, "=?utf-8?"), "кириллическое имя — encoded-word RFC 2047: %q", got.From)
	assert.True(t, strings.HasSuffix(got.From, " <noreply@example.ru>"), got.From)
	assert.Equal(t, "teacher@school.ru", got.To, "получатель — голый адрес")
	assert.Equal(t, "Подтверждение почты", got.Subject)
	assert.Equal(t, envelope().Text, got.Text)
	assert.Equal(t, envelope().HTML, got.HTML)
	assert.Equal(t, "prod", got.ConfigurationSet)
	assert.Equal(t, []string{"support@example.ru"}, got.ReplyTo)
	assert.Equal(t, map[string]string{
		"X-Trace":          "t-1",
		"List-Unsubscribe": "<mailto:unsub@example.ru>",
	}, got.Headers, "Message-ID и Reply-To в Headers[] не уходят")
}

func TestSend_FormatsFromWithoutName(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), nil)

	env := envelope()
	env.From = mail.Address{Email: "noreply@example.ru"}
	env.HTML = ""
	env.Headers = nil
	_, err := tr.Send(t.Context(), env)
	require.NoError(t, err)

	sent := srv.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, "noreply@example.ru", sent[0].From, "без имени — адрес без угловых скобок")
	assert.Empty(t, sent[0].HTML)
	assert.Empty(t, sent[0].Headers)
	assert.Empty(t, sent[0].ReplyTo)
}

func TestSend_ASCIINameIsQuoted(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), nil)

	env := envelope()
	env.From = mail.Address{Email: "noreply@example.ru", Name: "Rebar"}
	env.Headers = map[string]string{"Reply-To": "Support Team <support@example.ru>, ops@example.ru"}
	_, err := tr.Send(t.Context(), env)
	require.NoError(t, err)

	sent := srv.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, `"Rebar" <noreply@example.ru>`, sent[0].From, "net/mail всегда берёт имя в кавычки — законная quoted-string")
	assert.Equal(t, []string{`"Support Team" <support@example.ru>`, "ops@example.ru"}, sent[0].ReplyTo)
}

// Постоянный отказ провайдера → *mail.RejectedError с кодом и причиной
// провайдера; ни причина, ни текст ошибки не содержат тела письма.
func TestSend_RejectFor(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.RejectFor["teacher@school.ru"] = "MessageRejected"
	tr := newTransport(t, srv.URL(), nil)

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	require.True(t, mail.IsRejected(err), "MessageRejected — постоянный отказ: %v", err)
	var rej *mail.RejectedError
	require.ErrorAs(t, err, &rej)
	assert.Equal(t, "MessageRejected", rej.Code)
	assert.Contains(t, rej.Reason, "rejected by mailtest.SESServer")
	assert.NotContains(t, err.Error(), secretToken)
	assert.NotContains(t, err.Error(), testSecret)
	assert.Empty(t, srv.Sent())
}

// 429 — временный сбой: не RejectedError, письмо остаётся в очереди и уходит
// со следующей попытки.
func TestSend_ThrottleFor(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.ThrottleFor["teacher@school.ru"] = 1
	tr := newTransport(t, srv.URL(), nil)

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	assert.False(t, mail.IsRejected(err), "429 — не постоянный отказ: %v", err)
	var perr *sesv2.ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, http.StatusTooManyRequests, perr.Status)
	assert.Equal(t, "TooManyRequestsException", perr.Code)
	assert.NotContains(t, err.Error(), secretToken)

	_, err = tr.Send(t.Context(), envelope())
	require.NoError(t, err, "после паузы письмо уходит")
	assert.Len(t, srv.Sent(), 1)
}

// Неверный секрет → 403 → временный сбой, НЕ RejectedError.
//
// 403 говорит о нашей конфигурации (ключ отозван, ротация не доехала), а не о
// письме. Признай его постоянным — каждое письмо очереди легло бы в
// failed(rejected) с алертом «письма умирают», и после починки ключа их
// пришлось бы воскрешать руками. Как временный сбой очередь переждёт починку;
// если чинить долго — сработает MaxAttempts с честной причиной exhausted.
func TestSend_WrongSecretIsTemporary(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), func(c *sesv2.Config) { c.SecretAccessKey = "wrong-secret" })

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	assert.False(t, mail.IsRejected(err), "403 — ошибка конфигурации, не отказ письма: %v", err)
	var perr *sesv2.ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, http.StatusForbidden, perr.Status)
	assert.Equal(t, "InvalidSignatureException", perr.Code)
	assert.NotContains(t, err.Error(), testSecret)
	assert.NotContains(t, err.Error(), "wrong-secret")
	assert.Empty(t, srv.Sent(), "неподписанное письмо провайдер не принял")
}

func TestSend_WrongRegionIsTemporary(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), func(c *sesv2.Config) { c.Region = "us-east-1" })

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	assert.False(t, mail.IsRejected(err))
	var perr *sesv2.ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, http.StatusForbidden, perr.Status)
}

// Негодный Reply-To — постоянный отказ ещё до запроса: письмо с ним не уйдёт
// ни с какой попытки, а провайдер не должен видеть заведомо битый запрос.
func TestSend_InvalidReplyToIsRejected(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	tr := newTransport(t, srv.URL(), nil)

	env := envelope()
	env.Headers = map[string]string{"Reply-To": "not an address"}
	_, err := tr.Send(t.Context(), env)
	require.Error(t, err)
	var rej *mail.RejectedError
	require.ErrorAs(t, err, &rej)
	assert.Equal(t, "InvalidReplyTo", rej.Code)
	assert.NotContains(t, err.Error(), "not an address", "значение заголовка в ошибку не уходит")
	assert.Empty(t, srv.Sent())
}

// Таблица «код провайдера → класс ошибки». Каждый случай — свой ответ
// провайдера в одной из реальных форм: заголовок X-Amzn-ErrorType (AWS),
// поле Code в теле (Postbox), __type с namespace, код с URL после ':'.
func TestSend_ErrorClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		status       int
		headerCode   string
		body         string
		wantRejected bool
		wantCode     string
		wantReason   string
	}{
		{
			name: "MessageRejected via header", status: http.StatusBadRequest, headerCode: "MessageRejected",
			body: `{"message":"Email address is not verified."}`, wantRejected: true, wantCode: "MessageRejected",
			wantReason: "Email address is not verified.",
		},
		{
			name: "BadRequestException via body Code (Postbox)", status: http.StatusBadRequest,
			body: `{"Code":"BadRequestException","message":"sender is not allowed"}`, wantRejected: true,
			wantCode: "BadRequestException", wantReason: "sender is not allowed",
		},
		{
			name: "MailFromDomainNotVerifiedException via __type", status: http.StatusBadRequest,
			body:         `{"__type":"com.amazonaws.ses#MailFromDomainNotVerifiedException","message":"domain"}`,
			wantRejected: true, wantCode: "MailFromDomainNotVerifiedException",
		},
		{
			name: "AccountSuspendedException with URL suffix", status: http.StatusBadRequest,
			headerCode: "AccountSuspendedException:http://internal.amazon.com/coral/com.amazonaws.ses/",
			body:       `{"message":"suspended"}`, wantRejected: true, wantCode: "AccountSuspendedException",
		},
		{
			name: "NotFoundException 404", status: http.StatusNotFound, headerCode: "NotFoundException",
			body: `{"message":"Configuration set does not exist"}`, wantRejected: true, wantCode: "NotFoundException",
		},
		{
			name: "TooManyRequestsException 429", status: http.StatusTooManyRequests,
			body: `{"Code":"TooManyRequestsException","message":"quota"}`, wantCode: "TooManyRequestsException",
		},
		{
			name: "SendingPausedException is a pause, not a no", status: http.StatusBadRequest,
			headerCode: "SendingPausedException", body: `{"message":"paused"}`, wantCode: "SendingPausedException",
		},
		{
			name: "LimitExceededException", status: http.StatusBadRequest,
			headerCode: "LimitExceededException", body: `{"message":"limit"}`, wantCode: "LimitExceededException",
		},
		{
			name: "unknown 400 code retries", status: http.StatusBadRequest,
			headerCode: "ThrottlingException", body: `{"message":"slow down"}`, wantCode: "ThrottlingException",
		},
		{
			name: "403 InvalidSignatureException is config, retries", status: http.StatusForbidden,
			headerCode: "InvalidSignatureException", body: `{"message":"signature"}`, wantCode: "InvalidSignatureException",
		},
		{
			name: "unparseable 400 body retries", status: http.StatusBadRequest,
			body: `<html>bad gateway</html>`, wantCode: "",
		},
		{name: "500 InternalFailure", status: http.StatusInternalServerError, headerCode: "InternalFailure", body: `{"message":"x"}`, wantCode: "InternalFailure"},
		{name: "503 empty body", status: http.StatusServiceUnavailable, body: ``, wantCode: ""},
		{
			name: "permanent code in 5xx is somebody's failure, retries", status: http.StatusInternalServerError,
			headerCode: "MessageRejected", body: `{"message":"x"}`, wantCode: "MessageRejected",
		},
		{name: "200 without MessageId retries", status: http.StatusOK, body: `{}`},
		{name: "200 with garbage retries", status: http.StatusOK, body: `not json`},
		{name: "202 without body retries", status: http.StatusAccepted, body: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.headerCode != "" {
					w.Header().Set("X-Amzn-ErrorType", tc.headerCode)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			tr := newTransport(t, srv.URL, nil)

			_, err := tr.Send(t.Context(), envelope())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secretToken)
			if tc.wantRejected {
				var rej *mail.RejectedError
				require.ErrorAs(t, err, &rej, "ожидался постоянный отказ: %v", err)
				assert.Equal(t, tc.wantCode, rej.Code)
				if tc.wantReason != "" {
					assert.Equal(t, tc.wantReason, rej.Reason)
				}
				return
			}
			assert.False(t, mail.IsRejected(err), "ожидался временный сбой: %v", err)
			var perr *sesv2.ProviderError
			require.ErrorAs(t, err, &perr)
			assert.Equal(t, tc.status, perr.Status)
			assert.Equal(t, tc.wantCode, perr.Code)
		})
	}
}

// Ответ читается с лимитом: 200 KiB от провайдера не раздувают память, а код
// берётся из заголовка даже когда тело обрезано и не разбирается как JSON.
func TestSend_HugeResponseIsBounded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "MessageRejected")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"`+strings.Repeat("x", 200<<10)+`"}`)
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(t, srv.URL, nil)

	_, err := tr.Send(t.Context(), envelope())
	var rej *mail.RejectedError
	require.ErrorAs(t, err, &rej)
	assert.Equal(t, "MessageRejected", rej.Code)
	assert.Empty(t, rej.Reason, "обрезанный JSON не разбирается — сообщения нет")
}

// Сообщение провайдера — внешний ввод: переводы строк и управляющие символы
// схлопываются в пробелы, длина усекается. Оно уходит в LastError и в лог.
func TestSend_ReasonIsSingleLineAndTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("слово ", 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "MessageRejected")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"first\r\n\tsecond `+long+`"}`)
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(t, srv.URL, nil)

	_, err := tr.Send(t.Context(), envelope())
	var rej *mail.RejectedError
	require.ErrorAs(t, err, &rej)
	assert.True(t, strings.HasPrefix(rej.Reason, "first second слово"), rej.Reason)
	assert.LessOrEqual(t, len([]rune(rej.Reason)), 301)
	assert.True(t, strings.HasSuffix(rej.Reason, "…"))
	assert.NotContains(t, rej.Reason, "\n")
}

func TestSend_ContextTimeoutIsTemporary(t *testing.T) {
	t.Parallel()
	srv := stuckServer(t)
	tr := newTransport(t, srv.URL, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := tr.Send(ctx, envelope())
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, mail.IsRejected(err))
	assert.NotContains(t, err.Error(), secretToken)
}

// stuckServer — провайдер, который принял запрос и молчит до конца теста.
// release закрывается ДО srv.Close (Cleanup идёт в обратном порядке), иначе
// Close и обработчик ждали бы друг друга.
func stuckServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// Редирект не выполняется: подписанный запрос с телом письма не должен уехать
// на хост из Location. 3xx — временный сбой.
func TestSend_RedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	followed := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v2/email/outbound-emails", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(t, srv.URL, nil)

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	assert.False(t, mail.IsRejected(err))
	var perr *sesv2.ProviderError
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, http.StatusTemporaryRedirect, perr.Status)
	assert.Empty(t, followed, "запрос не должен уйти по Location")
}

func TestSend_UsesPathPrefixOfEndpoint(t *testing.T) {
	t.Parallel()
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"MessageId":"m-1"}`)
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(t, srv.URL+"/prefix/", nil)

	res, err := tr.Send(t.Context(), envelope())
	require.NoError(t, err)
	assert.Equal(t, "m-1", res.ProviderMessageID)
	assert.Equal(t, "/prefix/v2/email/outbound-emails", <-gotPath)
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*sesv2.Config)
		wantErr string
	}{
		{name: "ok https", mutate: func(*sesv2.Config) {}},
		{name: "ok http loopback ipv4", mutate: func(c *sesv2.Config) { c.Endpoint = "http://127.0.0.1:8080" }},
		{name: "ok http loopback ipv6", mutate: func(c *sesv2.Config) { c.Endpoint = "http://[::1]:8080" }},
		{name: "ok http localhost", mutate: func(c *sesv2.Config) { c.Endpoint = "http://localhost:8080/prefix" }},
		{name: "ok http insecure explicitly allowed", mutate: func(c *sesv2.Config) {
			c.Endpoint = "http://sesfake:8080"
			c.AllowInsecureEndpoint = true
		}},
		{name: "ok without configuration set", mutate: func(c *sesv2.Config) { c.ConfigurationSet = "" }},
		{name: "http non-loopback", mutate: func(c *sesv2.Config) { c.Endpoint = "http://postbox.cloud.yandex.net" }, wantErr: "https"},
		{name: "empty endpoint", mutate: func(c *sesv2.Config) { c.Endpoint = "" }, wantErr: "endpoint is empty"},
		{name: "relative endpoint", mutate: func(c *sesv2.Config) { c.Endpoint = "postbox.cloud.yandex.net" }, wantErr: "absolute URL"},
		{name: "ftp scheme", mutate: func(c *sesv2.Config) { c.Endpoint = "ftp://postbox.cloud.yandex.net" }, wantErr: "not supported"},
		{name: "endpoint with query", mutate: func(c *sesv2.Config) { c.Endpoint = "https://h.example?x=1" }, wantErr: "query"},
		{name: "endpoint with fragment", mutate: func(c *sesv2.Config) { c.Endpoint = "https://h.example#f" }, wantErr: "fragment"},
		{name: "endpoint with userinfo", mutate: func(c *sesv2.Config) { c.Endpoint = "https://user:pw@h.example" }, wantErr: "credentials"},
		{name: "endpoint path needs encoding", mutate: func(c *sesv2.Config) { c.Endpoint = "https://h.example/a b" }, wantErr: "path"},
		{name: "empty region", mutate: func(c *sesv2.Config) { c.Region = "" }, wantErr: "region"},
		{name: "uppercase region", mutate: func(c *sesv2.Config) { c.Region = "RU-CENTRAL1" }, wantErr: "region"},
		{name: "empty key id", mutate: func(c *sesv2.Config) { c.AccessKeyID = "" }, wantErr: "access key id"},
		{name: "key id with slash", mutate: func(c *sesv2.Config) { c.AccessKeyID = "AK/ID" }, wantErr: "access key id"},
		{name: "empty secret", mutate: func(c *sesv2.Config) { c.SecretAccessKey = "" }, wantErr: "secret access key"},
		{name: "configuration set with control char", mutate: func(c *sesv2.Config) { c.ConfigurationSet = "prod\n" }, wantErr: "configuration set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := sesv2.Config{
				Endpoint:         "https://postbox.cloud.yandex.net",
				Region:           testRegion,
				AccessKeyID:      testKeyID,
				SecretAccessKey:  testSecret,
				ConfigurationSet: "prod",
			}
			tc.mutate(&cfg)
			tr, err := sesv2.New(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.NotNil(t, tr)
				return
			}
			require.Error(t, err)
			assert.Nil(t, tr)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), testSecret, "секрет в ошибку конфигурации не попадает")
		})
	}
}

// Свой HTTP-клиент используется как есть — в том числе его таймаут.
func TestNew_UsesProvidedHTTPClient(t *testing.T) {
	t.Parallel()
	srv := stuckServer(t)
	tr := newTransport(t, srv.URL, func(c *sesv2.Config) {
		c.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	})

	_, err := tr.Send(t.Context(), envelope())
	require.Error(t, err)
	var netErr interface{ Timeout() bool }
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "ожидался таймаут клиента: %v", err)
	assert.False(t, mail.IsRejected(err))
}
