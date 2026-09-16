package outboxpg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/outbox/outboxtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// ОТМЕНА ПОСЛЕ ОТВЕТА ХЕНДЛЕРА НА ЖИВОЙ БАЗЕ. Ядро пишет исход мимо отмены
// прогона и возвращает невзятый остаток; двойник это подтверждает, но в базу не
// ходит — доезжает ли запрос pgx по отвязанному контексту и проходит ли возврат
// CHECK схемы, знает только база. Раньше Finish отменялся вместе с прогоном,
// строка ждала аренды, и хендлер исполнялся снова.
func TestDrain_CancelAfterHandlerAnswerRecordsOutcome(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	first := mustEnqueue(t, store, envelopeAt(now))
	now = now.Add(time.Second) // порядок Claim — по available_at
	second := mustEnqueue(t, store, envelopeAt(now))

	handler := outboxtest.NewRecordingHandler()
	worker := newWorker(t, store, handler, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.SetHook(func(context.Context, outbox.Delivery) error {
		cancel()
		return nil
	})

	processed, err := worker.Drain(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, outbox.ErrUnavailable, "исход или возврат до базы не доехал")
	assert.Equal(t, 2, processed, "исход первой строки и возврат второй")
	done := readRow(t, pool, first.ID)
	assert.Equal(t, string(outbox.StatusDone), done.Status, "исход хендлера до базы не доехал")
	assert.Nil(t, done.ClaimToken)
	assert.JSONEq(t, string(first.Payload), string(done.Payload), "payload стёрт")
	rest := readRow(t, pool, second.ID)
	assert.Equal(t, string(outbox.StatusPending), rest.Status, "невзятая строка не возвращена в очередь")
	assert.Zero(t, rest.Attempts, "остановка сожгла попытку")
	assert.Nil(t, rest.ClaimToken, "аренда не снята")

	handler.SetHook(nil)
	now = now.Add(drainConfig().Lease + time.Second)
	processed, err = worker.Drain(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, processed, "возвращённая строка не ушла следующим прогоном")
	assert.Equal(t, string(outbox.StatusDone), readRow(t, pool, second.ID).Status)
	handled := handler.Handled()
	require.Len(t, handled, 2, "хендлер отработавшей строки исполнился снова")
	assert.Equal(t, second.ID, handled[1].ID)
}

// ХЕНДЛЕР, ОБОРВАННЫЙ ОТМЕНОЙ, НА ЖИВОЙ БАЗЕ: строка остаётся в processing под
// своим токеном, а после аренды Claim отдаёт её с Reclaimed. MaxAttempts = 1:
// записанный исход увёл бы последнюю попытку в dead-letter.
func TestDrain_HandlerCutByCancelIsLeftForLease(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	cut := mustEnqueue(t, store, envelopeAt(now))
	now = now.Add(time.Second) // порядок Claim — по available_at
	rest := mustEnqueue(t, store, envelopeAt(now))

	handler := outboxtest.NewRecordingHandler()
	worker := newWorker(t, store, handler, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.SetHook(func(hctx context.Context, _ outbox.Delivery) error {
		cancel()
		<-hctx.Done()
		return hctx.Err()
	})

	processed, err := worker.Drain(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, outbox.ErrUnavailable, "возврат остатка до базы не доехал")
	assert.Equal(t, 1, processed, "возвращена только вторая строка")
	row := readRow(t, pool, cut.ID)
	assert.Equal(t, string(outbox.StatusProcessing), row.Status, "оборванный хендлер записан или возвращён")
	assert.NotNil(t, row.ClaimToken)
	assert.Equal(t, 1, row.Attempts)
	assert.Empty(t, row.LastError)
	assert.Equal(t, string(outbox.StatusPending), readRow(t, pool, rest.ID).Status)

	handler.SetHook(nil)
	now = now.Add(drainConfig().Lease + time.Second)
	processed, err = worker.Drain(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.Equal(t, string(outbox.StatusDone), readRow(t, pool, cut.ID).Status)
	var retried []bool
	for _, d := range handler.Handled() {
		if d.ID == cut.ID {
			retried = append(retried, d.Reclaimed)
		}
	}
	assert.Equal(t, []bool{false, true}, retried, "повтор оборванного хендлера не помечен Reclaimed")
}

// newWorker — воркер над адаптером: хендлер на testKind, управляемые часы. Все
// моменты уходят в базу параметром, поэтому аренда истекает по этим часам.
func newWorker(t *testing.T, store *outboxpg.Store, handler outbox.Handler, now func() time.Time) *outbox.Worker {
	t.Helper()
	reg := outbox.NewRegistry()
	reg.Register(testKind, handler)
	worker, err := outbox.NewWorker(store, reg, drainConfig())
	require.NoError(t, err)
	worker.SetClock(now)
	return worker
}

// drainConfig — политика воркера над живой базой.
func drainConfig() outbox.Config {
	return outbox.Config{
		Kinds:           []outbox.Kind{testKind},
		MaxAttempts:     1,
		Backoff:         outbox.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           time.Minute,
		HandlerTimeout:  10 * time.Second,
		BatchSize:       10,
		Retention:       time.Hour,
		MaxPayloadBytes: 64 << 10,
	}
}
