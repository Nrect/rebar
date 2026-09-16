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

// ВЗЯТАЯ С Reclaimed СТРОКА НЕ ВОЗВРАЩАЕТСЯ. Её прошлая попытка не досказала
// исход, и возврат стёр бы это знание: следующий Claim отдал бы её без
// Reclaimed, и хендлер не узнал бы, что эффект мог случиться. Она ждёт аренды;
// обычная строка той же пачки возвращается.
func TestDrain_ReleaseKeepsReclaimedRowUnderLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	lost := h.enqueue(t, nil)
	crashed, err := h.store.Claim(context.Background(), outbox.ClaimRequest{
		Now: h.clock.Now(), Lease: h.cfg.Lease, Limit: 1,
		Kinds: []outbox.Kind{kindPaid}, Token: uuid.New(),
	})
	require.NoError(t, err)
	require.Len(t, crashed, 1, "попытка упавшего воркера не взяла строку")
	h.clock.Advance(h.cfg.Lease + time.Second)
	fresh := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker, err := outbox.NewWorker(&cancellingStore{MemStore: h.store, cancel: cancel}, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)

	processed, err := worker.Drain(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, outbox.ErrUnavailable)
	assert.Equal(t, 1, processed, "возвращена только обычная строка")
	requireReleased(t, h.row(t, fresh.ID))
	kept := h.row(t, lost.ID)
	assert.Equal(t, outbox.StatusProcessing, kept.Status, "строка с Reclaimed возвращена в очередь")
	assert.Equal(t, 2, kept.Attempts)

	assert.Equal(t, 1, h.drain(t), "возвращённая строка не ушла следующим прогоном")
	h.clock.Advance(h.cfg.Lease + time.Second)
	assert.Equal(t, 1, h.drain(t))
	handled := h.handler.Handled()
	require.Len(t, handled, 2)
	assert.Equal(t, fresh.ID, handled[0].ID)
	assert.Equal(t, lost.ID, handled[1].ID)
	assert.True(t, handled[1].Reclaimed, "возврат стёр знание о неизвестном исходе")
}

// ОТМЕНА ПОСЛЕ ОТВЕТА ХЕНДЛЕРА НЕ СТОИТ ПОВТОРА ЭФФЕКТА. Хендлер отработал, и
// исход пишется мимо отмены: строка done и после Lease не исполняется снова.
// Невзятый остаток пачки возвращается в очередь и уходит следующим прогоном.
// Раньше Finish шёл по отменённому контексту и до хранилища не доезжал: строка
// ждала аренды, и хендлер повторялся на каждой штатной остановке.
func TestDrain_CancelAfterHandlerAnswerRecordsOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	first := h.enqueue(t, nil)
	h.clock.Advance(time.Second) // порядок Claim — по AvailableAt
	second := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.handler.SetHook(func(context.Context, outbox.Delivery) error {
		cancel()
		return nil
	})

	processed, err := h.worker.Drain(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, outbox.ErrUnavailable, "исход записан — сбоя хранилища нет")
	assert.Equal(t, 2, processed, "исход первой строки и возврат второй")
	row := h.row(t, first.ID)
	assert.Equal(t, outbox.StatusDone, row.Status, "исход хендлера не записан")
	assert.Nil(t, row.ClaimToken, "строка осталась под арендой")
	requireReleased(t, h.row(t, second.ID))

	h.handler.SetHook(nil)
	h.clock.Advance(h.cfg.Lease + time.Second)
	assert.Equal(t, 1, h.drain(t), "возвращённая строка не ушла следующим прогоном")
	assert.Equal(t, outbox.StatusDone, h.row(t, second.ID).Status)
	assert.Equal(t, []uuid.UUID{first.ID, second.ID}, handledIDs(h), "хендлер отработавшей строки исполнился снова")
}

// ОТВЕТ ХЕНДЛЕРА — НЕ ТОЛЬКО УСПЕХ. Пропуск, постоянный отказ и названный срок
// хендлер тоже вернул сам, осознанно, и отмена прогона их не отменяет: исход
// записан. Обрыв — только ошибка без класса (ниже).
func TestDrain_ClassifiedAnswerUnderCancelIsRecorded(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		answer  error
		status  outbox.Status
		reason  outbox.FailReason
		retryAt time.Time
	}{
		"пропуск": {answer: outbox.ErrSkip, status: outbox.StatusDone},
		"постоянный отказ": {
			answer: outbox.Permanent(outboxtest.ErrHandlerFailed),
			status: outbox.StatusFailed, reason: outbox.FailPermanent,
		},
		"названный срок": {
			answer: outbox.Throttled(outboxtest.ErrHandlerFailed, 5*time.Minute),
			status: outbox.StatusPending, retryAt: baseTime.Add(5 * time.Minute),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			env := h.enqueue(t, nil)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.handler.SetHook(func(context.Context, outbox.Delivery) error {
				cancel()
				return tc.answer
			})

			processed, err := h.worker.Drain(ctx)
			require.NoError(t, err, "единственная строка пачки: отмена после её исхода прогон не рвёт")
			assert.Equal(t, 1, processed)
			row := h.row(t, env.ID)
			assert.Equal(t, tc.status, row.Status, "ответ хендлера не записан")
			assert.Equal(t, tc.reason, row.FailReason)
			assert.Nil(t, row.ClaimToken, "строка осталась под арендой")
			assert.Equal(t, 1, row.Attempts, "ответ записан возвратом")
			if !tc.retryAt.IsZero() {
				assert.True(t, row.AvailableAt.Equal(tc.retryAt),
					"повтор назначен на %s, а не в названный срок %s", row.AvailableAt, tc.retryAt)
			}
		})
	}
}

// ХЕНДЛЕР, ОБОРВАННЫЙ ОТМЕНОЙ, ЯДРО НЕ ЗАПИСЫВАЕТ И НЕ ВОЗВРАЩАЕТ: эффект мог
// случиться. Записанный повтор стёр бы Reclaimed, а исчерпанная попытка увела
// бы строку в dead-letter, — поэтому строка ждёт конца аренды, как при падении
// процесса, и приходит с Reclaimed. Ошибка без класса и паника при отменённом
// прогоне — тот же обрыв: отличить их от вызванных отменой нельзя. Остаток
// пачки возвращается. MaxAttempts = 1: оборванная попытка последняя.
func TestDrain_HandlerCutByCancelIsLeftForLease(t *testing.T) {
	t.Parallel()
	cases := map[string]func(hctx context.Context, cancel context.CancelFunc) error{
		"ошибка контекста": func(hctx context.Context, cancel context.CancelFunc) error {
			cancel()
			<-hctx.Done()
			return hctx.Err()
		},
		"ошибка без класса": func(_ context.Context, cancel context.CancelFunc) error {
			cancel()
			return outboxtest.ErrHandlerFailed
		},
		"паника": func(_ context.Context, cancel context.CancelFunc) error {
			cancel()
			panic("outbox_test: хендлер упал после отмены")
		},
	}
	for name, cut := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(c *outbox.Config) { c.MaxAttempts = 1 })
			first := h.enqueue(t, nil)
			h.clock.Advance(time.Second) // порядок Claim — по AvailableAt
			second := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.handler.SetHook(func(hctx context.Context, _ outbox.Delivery) error {
				return cut(hctx, cancel)
			})

			processed, err := h.worker.Drain(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, outbox.ErrUnavailable)
			assert.Equal(t, 1, processed, "возвращена только вторая строка")
			row := h.row(t, first.ID)
			assert.Equal(t, outbox.StatusProcessing, row.Status, "оборванный хендлер записан или возвращён")
			assert.NotNil(t, row.ClaimToken)
			assert.Equal(t, 1, row.Attempts)
			assert.Empty(t, row.LastError)
			requireReleased(t, h.row(t, second.ID))

			// Аренда истекла — строка возвращается, и хендлер обязан узнать, что
			// прошлая попытка могла оставить эффект.
			h.handler.SetHook(nil)
			h.clock.Advance(h.cfg.Lease + time.Second)
			assert.Equal(t, 2, h.drain(t))
			assert.Equal(t, outbox.StatusDone, h.row(t, first.ID).Status)
			assert.Equal(t, outbox.StatusDone, h.row(t, second.ID).Status)
			reclaimed := map[uuid.UUID][]bool{}
			for _, d := range h.handler.Handled() {
				reclaimed[d.ID] = append(reclaimed[d.ID], d.Reclaimed)
			}
			assert.Equal(t, []bool{false, true}, reclaimed[first.ID], "повтор оборванного хендлера не помечен Reclaimed")
			assert.Equal(t, []bool{false}, reclaimed[second.ID])
		})
	}
}

// ПОВИСШАЯ ЗАПИСЬ ИСХОДА НЕ ПЕРЕЖИВАЕТ HandlerTimeout. Исход пишется мимо
// отмены прогона, и без своего срока повисшая база держала бы остановку
// процесса без предела.
func TestDrain_HungFinishDoesNotOutliveBudget(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	h := newHarness(t, func(c *outbox.Config) { c.HandlerTimeout = budget })
	h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.handler.SetHook(func(context.Context, outbox.Delivery) error {
		cancel()
		return nil
	})
	hung := &hungFinish{Store: h.store}
	worker, err := outbox.NewWorker(hung, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)

	processed, err := worker.Drain(ctx)

	hung.requireBudget(t, budget)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded, "запись исхода оборвал не её срок")
	assert.Zero(t, processed)
}

// ПОВИСШИЙ ВОЗВРАТ ОСТАТКА ТОЖЕ НЕ ПЕРЕЖИВАЕТ HandlerTimeout: возврат идёт мимо
// отмены, и без срока повисшая база держала бы остановку без предела.
func TestDrain_HungReleaseDoesNotOutliveBudget(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	h := newHarness(t, func(c *outbox.Config) { c.HandlerTimeout = budget })
	h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hung := &hungFinish{Store: &cancellingStore{MemStore: h.store, cancel: cancel}}
	worker, err := outbox.NewWorker(hung, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)

	processed, err := worker.Drain(ctx)

	hung.requireBudget(t, budget)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, outbox.ErrUnavailable, "сбой возврата потерян")
	require.ErrorIs(t, err, context.DeadlineExceeded, "возврат оборвал не его срок")
	assert.Zero(t, processed)
	assert.Empty(t, h.handler.Handled())
}

// Процесс убит между Handle и Finish: эффект хендлера случился, исход не
// записан. После аренды строка приходит снова, с Reclaimed.
func TestDrain_CrashBetweenHandleAndFinish(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	env := h.enqueue(t, nil)

	h.store.SetAfterHandle(func() {
		h.store.SetAfterHandle(nil) // «перезапуск»: следующий процесс хука не знает
		panic("kill -9 между Handle и Finish")
	})
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

	h.handler.SetHook(func(context.Context, outbox.Delivery) error {
		// Пока хендлер работал, аренда истекла и строки забрал другой воркер.
		h.clock.Advance(h.cfg.Lease + time.Second)
		_, err := h.store.Claim(context.Background(), outbox.ClaimRequest{
			Now: h.clock.Now(), Lease: h.cfg.Lease, Limit: 10,
			Kinds: []outbox.Kind{kindPaid}, Token: uuid.New(),
		})
		return err
	})

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
	h.store.SetFinishErr(errors.New("connection reset"))

	processed, err := h.worker.Drain(context.Background())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	assert.Equal(t, 0, processed)
	assert.Len(t, h.handler.Handled(), 1, "второй хендлер не звался")
}

// Сбой Claim — ошибка прогона, а не тихий ноль.
func TestDrain_ClaimFailureIsRunError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.store.SetErr(errors.New("connection reset"))

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

// requireReleased — строка возвращена в очередь: pending без аренды, попытка,
// взятая Claim, возвращена, исход не записан.
func requireReleased(t *testing.T, row outbox.Envelope) {
	t.Helper()
	assert.Equal(t, outbox.StatusPending, row.Status, "строка не возвращена в очередь")
	assert.Nil(t, row.ClaimToken, "аренда не снята")
	assert.Zero(t, row.Attempts, "попытка не возвращена")
	assert.Empty(t, row.LastError)
}

// handledIDs — идентификаторы доставок в порядке вызова хендлера.
func handledIDs(h *harness) []uuid.UUID {
	handled := h.handler.Handled()
	ids := make([]uuid.UUID, len(handled))
	for i, d := range handled {
		ids[i] = d.ID
	}
	return ids
}

// hungFinish — хранилище, у которого любая запись через Finish (исход или
// возврат) висит, пока её не оборвёт контекст. Контекст запоминается до
// ожидания, а без срока ожидания нет вовсе: сломанный срок роняет тест
// утверждением, а не просрочкой бинаря.
type hungFinish struct {
	outbox.Store

	called      bool
	enteredAt   time.Time
	entryErr    error
	deadline    time.Time
	hasDeadline bool
}

// hungCeiling — потолок ожидания hungFinish: выше бюджета теста, ниже
// просрочки бинаря.
const hungCeiling = 5 * time.Second

func (s *hungFinish) Finish(ctx context.Context, _ outbox.FinishRequest) error {
	s.called, s.enteredAt, s.entryErr = true, time.Now(), ctx.Err()
	s.deadline, s.hasDeadline = ctx.Deadline()
	if !s.hasDeadline {
		return errors.New("hungFinish: у записи нет срока")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hungCeiling):
		return errors.New("hungFinish: срок записи не сработал")
	}
}

// requireBudget — запись дошла до хранилища с контекстом, который отмена
// прогона не оборвала, и со сроком не дальше budget.
func (s *hungFinish) requireBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	require.True(t, s.called, "до записи в хранилище прогон не дошёл")
	require.NotErrorIs(t, s.entryErr, context.Canceled, "запись получила отменённый контекст прогона")
	require.True(t, s.hasDeadline, "у записи нет срока")
	assert.False(t, s.deadline.After(s.enteredAt.Add(budget)), "срок записи дальше HandlerTimeout")
}
