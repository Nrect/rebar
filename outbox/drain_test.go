package outbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

func TestDrain_HandledRowBecomesDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusDone, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Nil(t, row.ClaimToken, "аренда снята")
	assert.Nil(t, row.LockedUntil)
	require.NotNil(t, row.DoneAt)
	assert.Equal(t, baseTime, *row.DoneAt)
	assert.JSONEq(t, string(env.Payload), string(row.Payload), "payload остаётся")

	handled := h.handler.Handled()
	require.Len(t, handled, 1)
	assert.Equal(t, env.ID, handled[0].ID)
	assert.Equal(t, "A-42", handled[0].AggregateID)
	assert.False(t, handled[0].Reclaimed, "первая доставка ничего не переигрывает")
	assert.Equal(t, 1, handled[0].Attempts)
}

// Хендлер перепроверил предикат в момент выполнения (check-at-send) и эффекта
// нет: строка закрывается, а не считается отказом.
func TestDrain_SkipClosesRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.handler.SkipFor["A-42"] = true
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusDone, row.Status)
	assert.Empty(t, row.LastError, "пропуск не отказ")
	assert.Empty(t, row.FailReason)
}

// Постоянный отказ — failed без ретраев: повтор бессмысленен и вреден.
func TestDrain_PermanentIsTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.handler.PermanentFor["A-42"] = true
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusFailed, row.Status)
	assert.Equal(t, outbox.FailPermanent, row.FailReason)
	assert.Equal(t, 1, row.Attempts)
	assert.NotEmpty(t, row.LastError)

	h.clock.Advance(time.Hour)
	assert.Equal(t, 0, h.drain(t), "терминальная строка больше не берётся")
	assert.Len(t, h.handler.Handled(), 1)
}

// Названный срок повтора уважается: не раньше него, не позже Backoff.Max и не
// позже NotAfter.
func TestDrain_ThrottledRespectsNamedDelay(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		after    time.Duration
		notAfter time.Duration // от baseTime; ноль — без срока
		want     time.Duration
	}{
		"как просили":        {after: 5 * time.Minute, want: 5 * time.Minute},
		"обрезано потолком":  {after: 10 * time.Hour, want: time.Hour},
		"обрезано по сроку":  {after: 5 * time.Minute, notAfter: 2 * time.Minute, want: 2 * time.Minute},
		"отрицательный срок": {after: -time.Minute, want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			h.handler.ThrottleFor["A-42"] = tc.after
			env := h.enqueue(t, func(m *outbox.Message) {
				if tc.notAfter != 0 {
					m.NotAfter = baseTime.Add(tc.notAfter)
				}
			})

			assert.Equal(t, 1, h.drain(t))
			row := h.row(t, env.ID)
			assert.Equal(t, outbox.StatusPending, row.Status)
			assert.True(t, row.AvailableAt.Equal(baseTime.Add(tc.want)),
				"повтор назначен на %s, ожидался %s", row.AvailableAt, baseTime.Add(tc.want))
			assert.Equal(t, 1, row.Attempts)
		})
	}
}

// Throttling не жжёт лимит попыток: это «занято», а не «не могу».
func TestDrain_ThrottledDoesNotExhaustAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.MaxAttempts = 1 })
	h.handler.ThrottleFor["A-42"] = time.Minute
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 1, row.Attempts, "попытка потрачена, но строка жива")
}

func TestDrain_TemporaryFailureRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.handler.FailFor["A-42"] = 1
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Nil(t, row.ClaimToken)
	assert.NotEmpty(t, row.LastError)
	// Джиттер полный: нижняя граница включает сам now (задержка может быть нулём).
	assert.False(t, row.AvailableAt.Before(baseTime), "повтор не в прошлом")
	assert.False(t, row.AvailableAt.After(baseTime.Add(h.cfg.Backoff.Max)), "повтор не дальше Max")

	h.clock.Advance(h.cfg.Backoff.Max)
	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)
	assert.Len(t, h.handler.Handled(), 2)
}

func TestDrain_ExhaustedAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.MaxAttempts = 2 })
	h.handler.FailFor["A-42"] = 5
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusPending, h.row(t, env.ID).Status, "первая попытка ещё не последняя")

	h.clock.Advance(h.cfg.Backoff.Max)
	assert.Equal(t, 1, h.drain(t))

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusFailed, row.Status)
	assert.Equal(t, outbox.FailExhausted, row.FailReason)
	assert.Equal(t, 2, row.Attempts, "MaxAttempts считает и первую попытку")
	assert.NotEmpty(t, row.Payload, "payload в dead-letter остаётся: без него redrive невозможен")
}

// Срок вышел, пока строка ждала: хендлер не зовётся вовсе.
func TestDrain_ExpiredSkipsHandler(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, func(m *outbox.Message) { m.NotAfter = baseTime.Add(-time.Second) })

	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusExpired, h.row(t, env.ID).Status)
	assert.Empty(t, h.handler.Handled())
}

// Отложенная команда до срока не берётся.
func TestDrain_NotBeforeDelaysRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, func(m *outbox.Message) { m.NotBefore = baseTime.Add(time.Hour) })

	assert.Equal(t, 0, h.drain(t))
	assert.Equal(t, 0, h.row(t, env.ID).Attempts, "попытка не потрачена")

	h.clock.Advance(time.Hour)
	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)
}

// Паника хендлера — исход строки, а не падение воркера: временная ошибка,
// повтор, прогон продолжается.
func TestDrain_HandlerPanicIsTemporary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.handler.PanicFor["A-42"] = 1
	env := h.enqueue(t, nil)
	other := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	assert.Equal(t, 2, h.drain(t), "воркер жив и доделал пачку")

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Contains(t, row.LastError, "panic:")
	assert.Equal(t, outbox.StatusDone, h.row(t, other.ID).Status)
	assert.Equal(t, 1, h.handler.Panicked("A-42"))

	h.clock.Advance(h.cfg.Backoff.Max)
	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)
}

// Хендлер не уложился в HandlerTimeout — временный сбой, а не отказ.
func TestDrain_HandlerTimeoutIsTemporary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.HandlerTimeout = 20 * time.Millisecond })
	h.handler.Hook = func(ctx context.Context, _ outbox.Delivery) error {
		<-ctx.Done()
		return ctx.Err()
	}
	env := h.enqueue(t, nil)

	assert.Equal(t, 1, h.drain(t))
	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Contains(t, row.LastError, "deadline exceeded")
}

// Строку с типом, которого нет в реестре, не забирает никто: при выкате
// новой версии старый инстанс не утопит в dead-letter то, что умеет новая.
func TestDrain_UnknownKindIsNotClaimed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	legacy := h.enqueue(t, func(m *outbox.Message) { m.Kind = kindLegacy })
	known := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusDone, h.row(t, known.ID).Status)

	row := h.row(t, legacy.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 0, row.Attempts, "попытка не потрачена")

	stats, err := h.worker.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Unhandled, "строка видна отдельной метрикой, а не «очередь растёт»")
	assert.Equal(t, int64(1), stats.Pending)
}

// Ошибка выше уровня строки: хранилище отдало то, чего воркер не просил.
// Строка возвращается в pending, пачка останавливается.
func TestDrain_StoreReturningUnrequestedKindStopsBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, func(m *outbox.Message) { m.Kind = kindLegacy })

	worker, err := outbox.NewWorker(&sloppyStore{MemStore: h.store}, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)

	processed, err := worker.Drain(context.Background())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	assert.Equal(t, 1, processed, "строка возвращена, а не уведена в dead-letter")

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 0, row.Attempts)
	assert.Empty(t, h.handler.Handled())
}

// sloppyStore — хранилище, забывшее про фильтр по Kind.
type sloppyStore struct {
	*outboxtest.MemStore
}

func (s *sloppyStore) Claim(ctx context.Context, req outbox.ClaimRequest) ([]outbox.Envelope, error) {
	req.Kinds = append(req.Kinds, kindLegacy)
	return s.MemStore.Claim(ctx, req)
}
