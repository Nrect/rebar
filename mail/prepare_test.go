package mail_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
)

// nopStore и nopTransport — минимум, чтобы собрать Service для Prepare.
type nopStore struct{}

func (nopStore) Enqueue(context.Context, mail.Envelope) (mail.EnqueueResult, error) {
	return mail.EnqueueResult{}, nil
}
func (nopStore) Claim(context.Context, time.Time, time.Duration, int) ([]mail.Envelope, error) {
	return nil, nil
}
func (nopStore) Finish(context.Context, mail.FinishRequest) error     { return nil }
func (nopStore) Stats(context.Context, time.Time) (mail.Stats, error) { return mail.Stats{}, nil }
func (nopStore) Purge(context.Context, time.Time, int) (int, error)   { return 0, nil }

type nopTransport struct{}

func (nopTransport) Name() mail.TransportName { return "nop" }
func (nopTransport) Send(context.Context, mail.Envelope) (mail.SendResult, error) {
	return mail.SendResult{}, nil
}

func validConfig() mail.Config {
	return mail.Config{
		From:            mail.Address{Email: "noreply@example.ru", Name: "Пример"},
		Kinds:           []mail.Kind{"verify", "reset"},
		MessageIDDomain: "example.ru",
		MaxAttempts:     8,
		Backoff:         mail.Backoff{Base: 30 * time.Second, Max: time.Hour},
		Lease:           2 * time.Minute,
		SendTimeout:     30 * time.Second,
		BatchSize:       20,
		MinSendGap:      time.Second,
		Retention:       7 * 24 * time.Hour,
		MaxBodyBytes:    256 << 10,
		Uncertain:       mail.UncertainRetry,
	}
}

func newService(t *testing.T) *mail.Service {
	t.Helper()
	svc := mail.NewService(nopStore{}, nopTransport{}, nil, validConfig())
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) })
	return svc
}

func validMessage() mail.Message {
	return mail.Message{
		Kind:     "verify",
		To:       mail.Address{Email: "Teacher@School.RU", Name: "Учитель"},
		Subject:  "Подтверждение почты",
		Text:     "Ссылка: https://example.ru/verify?token=abc",
		HTML:     `<a href="https://example.ru/verify?token=abc">Подтвердить</a>`,
		Headers:  map[string]string{"reply-to": "support@example.ru", "X-Trace": "t-1"},
		DedupKey: " verify:abc ",
	}
}

func TestPrepare_NormalizesAndFingerprints(t *testing.T) {
	t.Parallel()
	svc := newService(t)

	env, err := svc.Prepare(validMessage())
	require.NoError(t, err)

	assert.Equal(t, "teacher@school.ru", env.To.Email, "адрес нормализован в нижний регистр")
	assert.Equal(t, "verify:abc", env.DedupKey, "ключ обрезан")
	assert.Equal(t, "noreply@example.ru", env.From.Email, "From — снапшот из Config")
	assert.Len(t, env.Fingerprint, 32)
	assert.Equal(t, "<"+env.ID.String()+"@example.ru>", env.MessageID)
	assert.Equal(t, mail.StatusPending, env.Status)
	assert.Contains(t, env.Headers, "Reply-To", "имя заголовка канонизировано")
	assert.NotContains(t, env.Headers, "reply-to")
	assert.Nil(t, env.NotAfter)
}

// Отпечаток совпадает для того же письма и различается при смене содержимого.
func TestPrepare_FingerprintStableAndSensitive(t *testing.T) {
	t.Parallel()
	svc := newService(t)

	a, err := svc.Prepare(validMessage())
	require.NoError(t, err)
	b, err := svc.Prepare(validMessage())
	require.NoError(t, err)
	assert.Equal(t, a.Fingerprint, b.Fingerprint, "то же письмо — тот же отпечаток")

	changed := validMessage()
	changed.Subject += "!"
	c, err := svc.Prepare(changed)
	require.NoError(t, err)
	assert.NotEqual(t, a.Fingerprint, c.Fingerprint, "другая тема — другой отпечаток")

	// Регистр адреса и порядок заголовков — то же письмо.
	same := validMessage()
	same.To.Email = "teacher@school.ru"
	same.Headers = map[string]string{"X-Trace": "t-1", "Reply-To": "support@example.ru"}
	d, err := svc.Prepare(same)
	require.NoError(t, err)
	assert.Equal(t, a.Fingerprint, d.Fingerprint)
}

func TestPrepare_RejectsHeaderInjection(t *testing.T) {
	t.Parallel()
	svc := newService(t)

	cases := map[string]func(*mail.Message){
		"CRLF в теме":           func(m *mail.Message) { m.Subject = "Привет\r\nBcc: victim@example.ru" },
		"LF в имени получателя": func(m *mail.Message) { m.To.Name = "Имя\nBcc: x@y.ru" },
		"CRLF в заголовке":      func(m *mail.Message) { m.Headers = map[string]string{"Reply-To": "a@b.ru\r\nBcc: c@d.ru"} },
		"структурный заголовок": func(m *mail.Message) { m.Headers = map[string]string{"Bcc": "c@d.ru"} },
		"From письмом":          func(m *mail.Message) { m.Headers = map[string]string{"From": "ceo@example.ru"} },
		"адрес с display-name":  func(m *mail.Message) { m.To.Email = "Учитель <t@school.ru>" },
		"два адреса":            func(m *mail.Message) { m.To.Email = "a@b.ru, c@d.ru" },
		"адрес без домена":      func(m *mail.Message) { m.To.Email = "teacher" },
		"пустая тема":           func(m *mail.Message) { m.Subject = "" },
		"пустой текст":          func(m *mail.Message) { m.Text = "" },
		"тело больше потолка":   func(m *mail.Message) { m.Text = strings.Repeat("x", 257<<10) },
		"заголовок длиннее 998": func(m *mail.Message) { m.Headers = map[string]string{"X-A": strings.Repeat("a", 999)} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := validMessage()
			mutate(&msg)
			_, err := svc.Prepare(msg)
			require.ErrorIs(t, err, mail.ErrInvalidMessage)
		})
	}
}

func TestPrepare_RejectsUnknownKindAndBadKey(t *testing.T) {
	t.Parallel()
	svc := newService(t)

	msg := validMessage()
	msg.Kind = "newsletter"
	_, err := svc.Prepare(msg)
	require.ErrorIs(t, err, mail.ErrBadKind)

	for name, key := range map[string]string{
		"пустой": "", "пробелы": "   ", "непечатный": "verify:\x00", "длинный": strings.Repeat("k", 201),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := validMessage()
			m.DedupKey = key
			_, err := svc.Prepare(m)
			require.ErrorIs(t, err, mail.ErrKeyInvalid)
		})
	}
}

func TestPrepare_KeepsNotAfterInUTC(t *testing.T) {
	t.Parallel()
	svc := newService(t)

	msk := time.FixedZone("MSK", 3*3600)
	msg := validMessage()
	msg.NotAfter = time.Date(2026, 9, 6, 15, 0, 0, 0, msk)

	env, err := svc.Prepare(msg)
	require.NoError(t, err)
	require.NotNil(t, env.NotAfter)
	assert.Equal(t, time.UTC, env.NotAfter.Location())
	assert.True(t, env.NotAfter.Equal(msg.NotAfter))
}

func TestNewService_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*mail.Config){
		"пустой From":                func(c *mail.Config) { c.From.Email = "" },
		"нет типов":                  func(c *mail.Config) { c.Kinds = nil },
		"тип с заглавной":            func(c *mail.Config) { c.Kinds = []mail.Kind{"Verify"} },
		"тип дважды":                 func(c *mail.Config) { c.Kinds = []mail.Kind{"verify", "verify"} },
		"домен Message-ID с @":       func(c *mail.Config) { c.MessageIDDomain = "a@b" },
		"ноль попыток":               func(c *mail.Config) { c.MaxAttempts = 0 },
		"аренда не длиннее таймаута": func(c *mail.Config) { c.Lease = c.SendTimeout },
		"Max меньше Base":            func(c *mail.Config) { c.Backoff.Max = c.Backoff.Base - 1 },
		"нулевой батч":               func(c *mail.Config) { c.BatchSize = 0 },
		"нулевая retention":          func(c *mail.Config) { c.Retention = 0 },
		"неизвестная политика":       func(c *mail.Config) { c.Uncertain = "maybe" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			mutate(&cfg)
			assert.Panics(t, func() { mail.NewService(nopStore{}, nopTransport{}, nil, cfg) })
		})
	}

	assert.Panics(t, func() { mail.NewService(nil, nopTransport{}, nil, validConfig()) })
	assert.Panics(t, func() { mail.NewService(nopStore{}, nil, nil, validConfig()) })
	assert.NotPanics(t, func() { mail.NewService(nopStore{}, nopTransport{}, nil, validConfig()) })
}

// Закрытые наборы перечисляют все значения: по ним CHECK адаптера и метки метрик.
func TestClosedSetsAreComplete(t *testing.T) {
	t.Parallel()
	assert.Len(t, mail.AllStatuses, 6)
	assert.Len(t, mail.AllFailReasons, 3)
	assert.Len(t, mail.AllFinishOutcomes, 5)
	assert.Len(t, mail.AllEnqueueOutcomes, 2)
	assert.Len(t, mail.AllSuppressReasons, 3)
	assert.Len(t, mail.AllUncertainPolicies, 2)
	for _, s := range mail.AllStatuses {
		if s == mail.StatusPending || s == mail.StatusSending {
			assert.False(t, s.Terminal(), s)
		} else {
			assert.True(t, s.Terminal(), s)
		}
	}
}

func TestRejectedError(t *testing.T) {
	t.Parallel()
	err := &mail.RejectedError{Code: "MessageRejected", Reason: "address is on the suppression list"}
	assert.True(t, mail.IsRejected(err))
	assert.False(t, mail.IsRejected(context.DeadlineExceeded))
	assert.Equal(t, "mail: rejected (MessageRejected): address is on the suppression list", err.Error())
}
