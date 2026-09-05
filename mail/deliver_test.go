package mail_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

func TestDeliver_SendsAndClearsBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))

	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusSent, row.Status)
	assert.Equal(t, mailtest.TransportName, row.Transport)
	assert.NotEmpty(t, row.ProviderMessageID)
	require.NotNil(t, row.SentAt)
	assert.Equal(t, baseTime, *row.SentAt)
	assert.Nil(t, row.LockedUntil)
	assert.Empty(t, row.Subject)
	assert.Empty(t, row.Text)
	assert.Empty(t, row.HTML)
	assert.Empty(t, row.Headers)

	sent := h.tr.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, env.ID, sent[0].ID)
	assert.Equal(t, "Подтверждение почты", sent[0].Subject, "транспорт получил письмо целиком")
}

// Постоянный отказ провайдера — failed без ретраев: повтор вреден.
func TestDeliver_RejectedIsTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.tr.RejectFor["teacher@school.ru"] = "MessageRejected"
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusFailed, row.Status)
	assert.Equal(t, mail.FailRejected, row.FailReason)
	assert.Equal(t, 1, row.Attempts)

	h.clock.advance(time.Hour)
	assert.Equal(t, 0, h.deliver(t), "терминальная строка больше не берётся")
	assert.Equal(t, 1, h.row(t, env.ID).Attempts)
	assert.Empty(t, h.tr.Sent())
}

func TestDeliver_TemporaryFailureRetriesThenSends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.tr.FailFor["teacher@school.ru"] = 1
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusPending, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Nil(t, row.LockedUntil)
	assert.NotEmpty(t, row.LastError)
	// Джиттер полный: нижняя граница включает сам now (delay может быть нулём).
	assert.False(t, row.NextAttemptAt.Before(baseTime), "повтор не в прошлом")
	assert.False(t, row.NextAttemptAt.After(baseTime.Add(h.cfg.Backoff.Max)), "повтор не дальше Max")
	assert.Empty(t, h.tr.Sent())

	h.clock.advance(h.cfg.Backoff.Max)
	assert.Equal(t, 1, h.deliver(t))
	assert.Equal(t, mail.StatusSent, h.row(t, env.ID).Status)
	assert.Len(t, h.tr.Sent(), 1)
}

func TestDeliver_ExhaustedAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.MaxAttempts = 2 })
	h.tr.FailFor["teacher@school.ru"] = 5
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	assert.Equal(t, mail.StatusPending, h.row(t, env.ID).Status, "первая попытка ещё не последняя")

	h.clock.advance(h.cfg.Backoff.Max)
	assert.Equal(t, 1, h.deliver(t))

	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusFailed, row.Status)
	assert.Equal(t, mail.FailExhausted, row.FailReason)
	assert.Equal(t, 2, row.Attempts, "MaxAttempts считает и первую попытку")
}

// Срок ссылки истёк до отправки — письмо не уходит вовсе.
func TestDeliver_ExpiredIsNotSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	env := h.enqueue(t, func(m *mail.Message) { m.NotAfter = baseTime.Add(-time.Second) })

	assert.Equal(t, 1, h.deliver(t))
	assert.Equal(t, mail.StatusExpired, h.row(t, env.ID).Status)
	assert.Empty(t, h.tr.Sent())
}

func TestDeliver_SuppressedIsNotSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	require.NoError(t, h.svc.Suppress(context.Background(), mail.Suppression{
		Email: "teacher@school.ru", Reason: mail.SuppressHardBounce,
	}))
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusSuppressed, row.Status)
	assert.Equal(t, string(mail.SuppressHardBounce), row.LastError)
	assert.Empty(t, h.tr.Sent())
}

// Стоп-лист недоступен — не проверили, значит не шлём; повтор позже.
func TestDeliver_SuppressorFailureRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	h.supp.Err = errors.New("suppression store is down")
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	assert.Equal(t, mail.StatusPending, h.row(t, env.ID).Status)
	assert.Empty(t, h.tr.Sent())
}

// Строка, взятая из sending с истёкшей арендой: исход прошлой попытки
// неизвестен, и политика решает, слать ли снова.
func TestDeliver_ReclaimedFollowsUncertainPolicy(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		policy mail.UncertainPolicy
		status mail.Status
		reason mail.FailReason
		sent   int
	}{
		"park":  {mail.UncertainPark, mail.StatusFailed, mail.FailUncertain, 0},
		"retry": {mail.UncertainRetry, mail.StatusSent, "", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = tc.policy })
			env := h.enqueue(t, nil)

			// Прогон, оборвавшийся между Claim и Finish: строка осталась в sending.
			claimed, err := h.store.Claim(context.Background(), h.clock.now(), h.cfg.Lease, 1)
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			require.False(t, claimed[0].Reclaimed, "живая аренда — ещё не потеря")

			h.clock.advance(h.cfg.Lease + time.Second)
			assert.Equal(t, 1, h.deliver(t))

			row := h.row(t, env.ID)
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.reason, row.FailReason)
			assert.Len(t, h.tr.Sent(), tc.sent)
			assert.Equal(t, 2, row.Attempts)
		})
	}
}

// namedTransport — декоратор вроде mailotel: имя пробрасывает, тип теряет.
type namedTransport struct{ mail.Transport }

// Без провайдера очередь не трогается: попытки не тратятся, письма ждут.
func TestDeliver_UnconfiguredLeavesQueueUntouched(t *testing.T) {
	t.Parallel()

	cases := map[string]mail.Transport{
		"напрямую":  mail.Unconfigured{},
		"декоратор": namedTransport{mail.Unconfigured{}},
	}
	for name, transport := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, nil)
			svc := mail.NewService(h.store, transport, nil, h.cfg)
			svc.SetClock(h.clock.now)
			env := h.enqueue(t, nil)

			processed, err := svc.Deliver(context.Background())
			require.NoError(t, err)
			assert.Equal(t, 0, processed)

			row := h.row(t, env.ID)
			assert.Equal(t, mail.StatusPending, row.Status)
			assert.Equal(t, 0, row.Attempts, "попытка не потрачена")
			assert.Nil(t, row.LockedUntil, "Claim не звали")
		})
	}
}

// Транспорт не уложился в SendTimeout — временный сбой, а не отказ.
func TestDeliver_SendTimeoutIsTemporary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.SendTimeout = 20 * time.Millisecond })
	h.tr.SendHook = func(ctx context.Context, _ mail.Envelope) (mail.SendResult, error) {
		<-ctx.Done()
		return mail.SendResult{}, ctx.Err()
	}
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusPending, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Contains(t, row.LastError, "deadline exceeded")
}

// Длинная ошибка провайдера режется по границе руны: колонка остаётся UTF-8.
func TestDeliver_TruncatesLongError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	// Трёхбайтовая руна: 500 не делится на 3, обрезка обязана отступить назад.
	h.tr.SendHook = func(context.Context, mail.Envelope) (mail.SendResult, error) {
		return mail.SendResult{}, errors.New(strings.Repeat("→", 400))
	}
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.deliver(t))
	got := h.row(t, env.ID).LastError
	assert.LessOrEqual(t, len(got), mail.MaxErrorLen)
	assert.True(t, utf8.ValidString(got), "усечённая ошибка обязана остаться валидным UTF-8")
	assert.Equal(t, strings.Repeat("→", len(got)/3), got, "обрезано ровно по руне")
}
