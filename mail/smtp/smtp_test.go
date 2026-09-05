package smtp_test

import (
	"context"
	"io"
	"net"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/smtp"
)

// secretToken — не должен оказаться в тексте ошибки.
const secretToken = "SECRET-TOKEN-42"

// plainConfig — конфиг для fakeServer; AllowPlaintext, потому что у фейка нет TLS.
func plainConfig(t *testing.T, s *fakeServer) smtp.Config {
	t.Helper()
	host, port := s.hostPort(t)
	return smtp.Config{Host: host, Port: port, TLS: smtp.TLSNone, AllowPlaintext: true, Timeout: 3 * time.Second}
}

func newTransport(t *testing.T, cfg smtp.Config) *smtp.Transport {
	t.Helper()
	tr, err := smtp.New(cfg)
	require.NoError(t, err)
	return tr
}

// envelope — конверт в том виде, в каком его отдаёт Prepare.
func envelope() mail.Envelope {
	id := uuid.New()
	return mail.Envelope{
		ID:        id,
		Kind:      "verify",
		To:        mail.Address{Email: "teacher@school.ru", Name: "Teacher Name"},
		From:      mail.Address{Email: "noreply@example.ru", Name: "Example Team"},
		Subject:   "Please confirm your email",
		Text:      "Link: https://example.ru/verify?token=" + secretToken,
		HTML:      `<a href="https://example.ru/verify?token=` + secretToken + `">Confirm</a>`,
		Headers:   map[string]string{"Reply-To": "support@example.ru", "X-Trace": "trace-1"},
		MessageID: "<" + id.String() + "@example.ru>",
		Status:    mail.StatusSending,
	}
}

// assertNoContentLeak — ни адрес, ни тема, ни тело, ни Message-ID в тексте ошибки.
func assertNoContentLeak(t *testing.T, err error, env mail.Envelope) {
	t.Helper()
	text := err.Error()
	for _, secret := range []string{env.To.Email, env.From.Email, env.Subject, secretToken, strings.Trim(env.MessageID, "<>")} {
		if secret != "" { // конверт без адреса: пустую строку содержит любой текст
			assert.NotContains(t, text, secret)
		}
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	valid := smtp.Config{Host: "smtp.example.ru", Port: 587, Timeout: 10 * time.Second}
	cases := map[string]func(c *smtp.Config){
		"пустой host":                 func(c *smtp.Config) { c.Host = "" },
		"нулевой порт":                func(c *smtp.Config) { c.Port = 0 },
		"порт вне диапазона":          func(c *smtp.Config) { c.Port = 70000 },
		"нулевой таймаут":             func(c *smtp.Config) { c.Timeout = 0 },
		"TLS none без AllowPlaintext": func(c *smtp.Config) { c.TLS = smtp.TLSNone },
		"неизвестный TLS":             func(c *smtp.Config) { c.TLS = "ssl" },
		"неизвестный auth":            func(c *smtp.Config) { c.Auth = "cram-md5" },
		"пароль при auth none":        func(c *smtp.Config) { c.Password = "p" },
		"логин при auth none":         func(c *smtp.Config) { c.Username = "u" },
		"auth plain без пароля": func(c *smtp.Config) {
			c.Auth = smtp.AuthPlain
			c.Username = "u"
		},
		"auth login без логина": func(c *smtp.Config) {
			c.Auth = smtp.AuthLogin
			c.Password = "p"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			mutate(&cfg)
			tr, err := smtp.New(cfg)
			require.ErrorIs(t, err, smtp.ErrInvalidConfig)
			assert.Nil(t, tr)
		})
	}
}

// Каждая пара AllTLSModes × AllAuthModes собирается; пустые TLS и Auth — умолчания.
func TestNew_AcceptsEveryModePair(t *testing.T) {
	t.Parallel()
	base := smtp.Config{Host: "smtp.example.ru", Port: 587, Timeout: time.Second}
	for _, tlsMode := range smtp.AllTLSModes {
		for _, authMode := range smtp.AllAuthModes {
			cfg := base
			cfg.TLS, cfg.Auth = tlsMode, authMode
			cfg.AllowPlaintext = tlsMode == smtp.TLSNone
			if authMode != smtp.AuthNone {
				cfg.Username, cfg.Password = "user", "pass"
			}
			_, err := smtp.New(cfg)
			require.NoError(t, err, "tls=%s auth=%s", tlsMode, authMode)
		}
	}
	_, err := smtp.New(base)
	require.NoError(t, err)
}

func TestTransport_Name(t *testing.T) {
	t.Parallel()
	tr := newTransport(t, smtp.Config{Host: "smtp.example.ru", Port: 587, Timeout: time.Second})
	assert.Equal(t, mail.TransportName("smtp"), tr.Name())
	assert.Equal(t, smtp.Name, tr.Name())
}

func TestSend_DeliversEnvelopeAndReturnsQueueID(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{})
	tr := newTransport(t, plainConfig(t, srv))
	env := envelope()

	res, err := tr.Send(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, fakeQueueID, res.ProviderMessageID)

	got, err := netmail.ReadMessage(strings.NewReader(srv.received()))
	require.NoError(t, err)
	assert.Equal(t, env.MessageID, got.Header.Get("Message-ID"), "Message-ID из конверта, а не сгенерированный go-mail")
	assert.Equal(t, env.Headers["Reply-To"], got.Header.Get("Reply-To"))
	assert.Equal(t, env.Headers["X-Trace"], got.Header.Get("X-Trace"))
	assert.Equal(t, env.Subject, got.Header.Get("Subject"))
	assert.NotEmpty(t, got.Header.Get("Date"))

	from, err := netmail.ParseAddress(got.Header.Get("From"))
	require.NoError(t, err)
	assert.Equal(t, env.From.Email, from.Address)
	assert.Equal(t, env.From.Name, from.Name)
	to, err := netmail.ParseAddress(got.Header.Get("To"))
	require.NoError(t, err)
	assert.Equal(t, env.To.Email, to.Address)
	assert.Equal(t, env.To.Name, to.Name)
	assert.Empty(t, got.Header.Get("Cc"))
	assert.Empty(t, got.Header.Get("Bcc"))

	assert.Contains(t, got.Header.Get("Content-Type"), "multipart/alternative")
	body, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "text/plain")
	assert.Contains(t, string(body), "text/html")
	assert.Equal(t, 2, strings.Count(string(body), secretToken), "токен в обеих частях")
}

func TestSend_TextOnlyIsNotMultipart(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{})
	tr := newTransport(t, plainConfig(t, srv))
	env := envelope()
	env.HTML = ""

	_, err := tr.Send(context.Background(), env)
	require.NoError(t, err)

	got, err := netmail.ReadMessage(strings.NewReader(srv.received()))
	require.NoError(t, err)
	assert.Contains(t, got.Header.Get("Content-Type"), "text/plain")
	assert.NotContains(t, got.Header.Get("Content-Type"), "multipart")
}

// Кавычка в display-name не протаскивает второй адрес: имя экранирует net/mail.
func TestSend_QuotesInDisplayNameAreEscaped(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{})
	tr := newTransport(t, plainConfig(t, srv))
	env := envelope()
	env.To.Name = `Evil" <attacker@evil.ru>, "Name`

	_, err := tr.Send(context.Background(), env)
	require.NoError(t, err)

	got, err := netmail.ReadMessage(strings.NewReader(srv.received()))
	require.NoError(t, err)
	addrs, err := netmail.ParseAddressList(got.Header.Get("To"))
	require.NoError(t, err)
	require.Len(t, addrs, 1, "второй адрес через имя не протаскивается")
	assert.Equal(t, env.To.Email, addrs[0].Address)
	assert.Equal(t, env.To.Name, addrs[0].Name)
	assert.True(t, srv.saw("RCPT"))
}

func TestSend_PermanentRejectsBecomeRejectedError(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		sc   scenario
		code string
		esc  string
	}{
		"550 на RCPT TO": {
			sc:   scenario{rcptReply: "550 5.1.1 <teacher@school.ru>: Recipient address rejected: User unknown"},
			code: "550", esc: "5.1.1",
		},
		"554 на DATA": {
			sc:   scenario{dataReply: "554 5.7.1 Message rejected as spam"},
			code: "554", esc: "5.7.1",
		},
		"552 на DATA без расширенного кода": {
			sc:   scenario{dataReply: "552 Message size exceeds fixed limit"},
			code: "552",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startFakeServer(t, tc.sc)
			tr := newTransport(t, plainConfig(t, srv))
			env := envelope()

			_, err := tr.Send(context.Background(), env)

			var rej *mail.RejectedError
			require.ErrorAs(t, err, &rej)
			assert.True(t, mail.IsRejected(err))
			assert.Equal(t, tc.code, rej.Code)
			if tc.esc != "" {
				assert.Contains(t, rej.Reason, tc.esc)
			}
			assertNoContentLeak(t, err, env)
		})
	}
}

// Конверт без адреса — RejectedError до транзакции.
func TestSend_MissingAddressIsRejected(t *testing.T) {
	t.Parallel()
	cases := map[string]func(e *mail.Envelope){
		"без получателя":  func(e *mail.Envelope) { e.To = mail.Address{} },
		"без отправителя": func(e *mail.Envelope) { e.From = mail.Address{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startFakeServer(t, scenario{})
			tr := newTransport(t, plainConfig(t, srv))
			env := envelope()
			mutate(&env)

			_, err := tr.Send(context.Background(), env)

			var rej *mail.RejectedError
			require.ErrorAs(t, err, &rej)
			assert.Empty(t, rej.Code, "код сервера тут ни при чём")
			assert.False(t, srv.saw("MAIL"), "до транзакции дело не дошло")
			assertNoContentLeak(t, err, env)
		})
	}
}

func TestSend_TransientFailuresStayRetryable(t *testing.T) {
	t.Parallel()
	cases := map[string]scenario{
		"450 на RCPT TO": {rcptReply: "450 4.2.0 <teacher@school.ru>: Recipient address rejected: Greylisted"},
		"451 на DATA":    {dataReply: "451 4.3.0 Temporary failure, try again later"},
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startFakeServer(t, sc)
			tr := newTransport(t, plainConfig(t, srv))
			env := envelope()

			_, err := tr.Send(context.Background(), env)

			require.Error(t, err)
			assert.False(t, mail.IsRejected(err), "4xx — временный сбой, ядро повторит")
			assertNoContentLeak(t, err, env)
		})
	}
}

func TestSend_DialFailureIsTransient(t *testing.T) {
	t.Parallel()
	// Слушатель закрывается сразу: порт свободен, соединение отклоняется.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port := splitHostPort(t, ln.Addr().String())
	require.NoError(t, ln.Close())
	tr := newTransport(t, smtp.Config{Host: host, Port: port, TLS: smtp.TLSNone, AllowPlaintext: true, Timeout: 2 * time.Second})

	_, err = tr.Send(context.Background(), envelope())

	require.Error(t, err)
	assert.False(t, mail.IsRejected(err))
}

// 250 на DATA — письмо ушло; ошибка RSET после этого не повод повторять.
func TestSend_ErrorAfterAcceptedDataIsSuccess(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{rsetReply: "500 5.5.1 RSET not welcome here"})
	tr := newTransport(t, plainConfig(t, srv))

	res, err := tr.Send(context.Background(), envelope())

	require.NoError(t, err)
	assert.Equal(t, fakeQueueID, res.ProviderMessageID)
}

// Сервер молчит на DATA; дедлайн сокета 5 с, ctx 300 мс — Send возвращается по ctx.
func TestSend_ContextDeadlineWinsOverTimeout(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{hangOnData: true})
	cfg := plainConfig(t, srv)
	cfg.Timeout = 5 * time.Second
	tr := newTransport(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := tr.Send(ctx, envelope())

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second, "вернулись по ctx, не по дедлайну сокета")
	assert.False(t, mail.IsRejected(err))
}

// Пустой Config.TLS — mandatory: сервер без STARTTLS письма не получает.
func TestSend_DefaultTLSIsMandatory(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{})
	host, port := srv.hostPort(t)
	tr := newTransport(t, smtp.Config{Host: host, Port: port, Timeout: 2 * time.Second})

	_, err := tr.Send(context.Background(), envelope())

	require.Error(t, err)
	assert.False(t, mail.IsRejected(err), "нет STARTTLS — сбой соединения, не отказ письма")
	assert.True(t, srv.saw("EHLO"))
	assert.False(t, srv.saw("MAIL"), "письмо не должно уйти в открытую")
}

// Откат STARTTLS без AllowPlaintext: AUTH не начинается, пароль остаётся у нас.
// Хост 0.0.0.0, а не 127.0.0.1: для localhost/127.0.0.1/::1 go-mail (как и
// stdlib) пароль по открытому соединению отдаёт; 0.0.0.0 ведёт на loopback.
func TestSend_PasswordStaysHomeWhenTLSFallsBack(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{advertiseAuth: true})
	_, port := srv.hostPort(t)
	tr := newTransport(t, smtp.Config{
		Host: "0.0.0.0", Port: port, TLS: smtp.TLSOpportunistic,
		Auth: smtp.AuthPlain, Username: "user", Password: "hunter2",
		Timeout: 2 * time.Second,
	})

	_, err := tr.Send(context.Background(), envelope())

	require.Error(t, err)
	assert.False(t, mail.IsRejected(err))
	assert.True(t, srv.saw("EHLO"))
	assert.False(t, srv.saw("AUTH"), "PLAIN по открытому соединению без AllowPlaintext запрещён")
	assert.False(t, srv.saw("MAIL"))
}

// С AllowPlaintext тот же сценарий проходит (NOENC).
func TestSend_AuthOverPlaintextWhenAllowed(t *testing.T) {
	t.Parallel()
	srv := startFakeServer(t, scenario{advertiseAuth: true})
	_, port := srv.hostPort(t)
	tr := newTransport(t, smtp.Config{
		Host: "0.0.0.0", Port: port, TLS: smtp.TLSNone, AllowPlaintext: true,
		Auth: smtp.AuthPlain, Username: "user", Password: "hunter2",
		Timeout: 2 * time.Second,
	})

	res, err := tr.Send(context.Background(), envelope())

	require.NoError(t, err)
	assert.Equal(t, fakeQueueID, res.ProviderMessageID)
	assert.True(t, srv.saw("AUTH"))
}
