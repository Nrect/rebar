package outbox_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// Отмена пришла до старта хендлера: вся оставшаяся пачка возвращается в
// pending немедленно и БЕЗ потраченной попытки. Быстрая остановка (выкат,
// SIGTERM) не должна приближать работу к dead-letter.
func TestDrain_CancelBeforeHandlerReleasesBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	first := h.enqueue(t, nil)
	second := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker, err := outbox.NewWorker(&cancellingStore{MemStore: h.store, cancel: cancel}, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)

	processed, err := worker.Drain(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, processed, "исход released записан обеим строкам")
	assert.Empty(t, h.handler.Handled(), "хендлер не звался ни разу")

	for _, env := range []outbox.Envelope{first, second} {
		row := h.row(t, env.ID)
		assert.Equal(t, outbox.StatusPending, row.Status)
		assert.Equal(t, 0, row.Attempts, "попытка возвращена")
		assert.Nil(t, row.ClaimToken, "аренда снята")
		assert.True(t, row.AvailableAt.Equal(baseTime), "строка доступна сразу")
	}
}

// cancellingStore — отмена приходит между Claim и первой строкой: ровно то
// окно, ради которого заведён исход released.
type cancellingStore struct {
	*outboxtest.MemStore
	cancel context.CancelFunc
}

func (s *cancellingStore) Claim(ctx context.Context, req outbox.ClaimRequest) ([]outbox.Envelope, error) {
	rows, err := s.MemStore.Claim(ctx, req)
	s.cancel()
	return rows, err
}

// Отмена застала хендлер за работой: исход записать уже нечем, строка
// остаётся под арендой и возвращается после её истечения с Reclaimed.
func TestDrain_CancelDuringHandlerLeavesRowForLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.handler.Hook = func(context.Context, outbox.Delivery) error {
		cancel()
		return nil
	}

	processed, err := h.worker.Drain(ctx)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, processed)

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusProcessing, row.Status, "строка ждёт истечения аренды")
	assert.NotNil(t, row.ClaimToken)

	// Аренда истекла — строка возвращается, и хендлер обязан узнать, что
	// прошлая попытка могла оставить эффект.
	h.handler.Hook = nil
	h.clock.Advance(h.cfg.Lease + time.Second)
	assert.Equal(t, 1, h.drain(t))

	handled := h.handler.Handled()
	require.Len(t, handled, 2)
	assert.False(t, handled[0].Reclaimed)
	assert.True(t, handled[1].Reclaimed, "повтор после потерянной аренды помечен")
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)
	assert.Equal(t, 2, h.row(t, env.ID).Attempts)
}

// Процесс убит между Handle и Finish: эффект хендлера случился, исход не
// записан. После аренды строка приходит снова, с Reclaimed.
func TestDrain_CrashBetweenHandleAndFinish(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, nil)

	h.store.AfterHandle = func() {
		h.store.AfterHandle = nil // «перезапуск»: следующий процесс хука не знает
		panic("kill -9 между Handle и Finish")
	}
	assert.Panics(t, func() { _, _ = h.worker.Drain(context.Background()) })

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusProcessing, row.Status, "исход не записан")
	assert.Equal(t, 1, row.Attempts)
	require.Len(t, h.handler.Handled(), 1, "эффект хендлера уже случился")

	h.clock.Advance(h.cfg.Lease + time.Second)
	assert.Equal(t, 1, h.drain(t))

	handled := h.handler.Handled()
	require.Len(t, handled, 2)
	assert.True(t, handled[1].Reclaimed,
		"вторая доставка обязана сказать хендлеру, что эффект мог случиться")
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)
}

// Аренду перехватил другой воркер: Finish со старым токеном не меняет ничего,
// пачка останавливается. Иначе проснувшийся воркер A переписал бы результат B.
func TestDrain_LostClaimStopsBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.enqueue(t, nil)
	h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	h.handler.Hook = func(context.Context, outbox.Delivery) error {
		// Пока хендлер работал, аренда истекла и строки забрал другой воркер.
		h.clock.Advance(h.cfg.Lease + time.Second)
		_, err := h.store.Claim(context.Background(), outbox.ClaimRequest{
			Now: h.clock.Now(), Lease: h.cfg.Lease, Limit: 10,
			Kinds: []outbox.Kind{kindPaid}, Token: uuid.New(),
		})
		return err
	}

	processed, err := h.worker.Drain(context.Background())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	require.ErrorIs(t, err, outbox.ErrClaimLost)
	assert.Equal(t, 0, processed)
	assert.Len(t, h.handler.Handled(), 1, "вторая строка не бралась в работу")
}

// Исход записать не удалось — остаток пачки не идёт: работать дальше значит
// плодить эффекты, чей исход тоже некуда записать.
func TestDrain_FinishFailureStopsBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.enqueue(t, nil)
	h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })
	h.store.FinishErr = errors.New("connection reset")

	processed, err := h.worker.Drain(context.Background())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	assert.Equal(t, 0, processed)
	assert.Len(t, h.handler.Handled(), 1, "второй хендлер не звался")
}

// Сбой Claim — ошибка прогона, а не тихий ноль.
func TestDrain_ClaimFailureIsRunError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.store.Err = errors.New("connection reset")

	processed, err := h.worker.Drain(context.Background())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	assert.Equal(t, 0, processed)
}

// Два воркера над одним хранилищем: аренда и токены обязаны не дать
// выполнить строку дважды.
func TestDrain_ConcurrentWorkersHandleEachRowOnce(t *testing.T) {
	t.Parallel()
	const rows = 200

	h := newHarness(t, nil)
	second, err := outbox.NewWorker(h.store, h.reg, h.cfg)
	require.NoError(t, err)
	second.SetClock(h.clock.Now)
	for i := range rows {
		h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "A-" + strconv.Itoa(i) })
	}

	var wg sync.WaitGroup
	for _, w := range []*outbox.Worker{h.worker, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainAll(t, w)
		}()
	}
	wg.Wait()

	handled := h.handler.Handled()
	assert.Len(t, handled, rows, "каждая строка выполнена ровно один раз")
	seen := make(map[uuid.UUID]bool, len(handled))
	for _, d := range handled {
		assert.False(t, seen[d.ID], "строка %s выполнена дважды", d.ID)
		seen[d.ID] = true
	}
}

// drainAll гоняет Drain, пока очередь не опустеет; потолок прогонов не даёт
// тесту зависнуть, если аренда сломана.
func drainAll(t *testing.T, w *outbox.Worker) {
	t.Helper()

	ctx := context.Background()
	for range 500 {
		if _, err := w.Drain(ctx); err != nil {
			t.Error(err)
			return
		}
		stats, err := w.Stats(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		if stats.Pending == 0 && stats.Processing == 0 {
			return
		}
	}
	t.Error("очередь не опустела за 500 прогонов")
}
