package mail_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
)

// Исход записать не удалось — остаток пачки не идёт: слать дальше значит
// плодить дубли, чей исход тоже некуда записать.
func TestDeliver_FinishFailureStopsBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.enqueue(t, nil)
	h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })
	h.store.SetFinishErr(errors.New("connection reset"))

	processed, err := h.svc.Deliver(context.Background())
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, 0, processed)
	assert.Len(t, h.tr.Sent(), 1, "вторая строка не отправлена")
}

// ОТМЕНА ПОСРЕДИ ОТПРАВКИ НЕ СТОИТ ДУБЛЯ. Письмо ушло, и исход пишется мимо
// отмены: строка sent и больше не шлётся. Невзятый остаток пачки возвращается
// в очередь без потраченной попытки и уходит следующим прогоном — не дожидаясь
// Lease и не в failed при UncertainPark. Раньше Finish шёл по отменённому
// контексту: при UncertainRetry письмо уезжало второй раз, при UncertainPark
// вся пачка, включая доставленное, уходила в failed.
func TestDeliver_CancelDuringSendRecordsOutcome(t *testing.T) {
	t.Parallel()
	for _, policy := range mail.AllUncertainPolicies {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = policy })
			first := h.enqueue(t, nil)
			h.clock.advance(time.Second) // порядок Claim — по NextAttemptAt
			second := h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var sent []uuid.UUID
			h.tr.SetSendHook(func(_ context.Context, env mail.Envelope) (mail.SendResult, error) {
				sent = append(sent, env.ID)
				cancel()
				return mail.SendResult{ProviderMessageID: "mem-" + env.ID.String()}, nil
			})

			processed, err := h.svc.Deliver(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, mail.ErrUnavailable, "исход записан — сбоя хранилища нет")
			assert.Equal(t, 2, processed, "исход отправленной строки и возврат второй")
			row := h.row(t, first.ID)
			assert.Equal(t, mail.StatusSent, row.Status, "исход отправленного письма не записан")
			assert.Equal(t, "mem-"+first.ID.String(), row.ProviderMessageID)
			requireReleased(t, h.row(t, second.ID))

			assert.Equal(t, 1, h.deliver(t), "возвращённая строка не ушла следующим прогоном")
			assert.Equal(t, mail.StatusSent, h.row(t, first.ID).Status)
			assert.Equal(t, mail.StatusSent, h.row(t, second.ID).Status)
			assert.Equal(t, []uuid.UUID{first.ID, second.ID}, sent, "письмо ушло второй раз")
		})
	}
}

// ОТМЕНА МЕЖДУ ПИСЬМАМИ ВОЗВРАЩАЕТ ОСТАТОК ПАЧКИ. Claim перевёл в sending всю
// пачку; без возврата невзятые строки ждали бы Lease, приходили с Reclaimed и
// при UncertainPark уходили в failed, ни разу не отправившись, а при
// UncertainRetry — с потраченной попыткой.
func TestDeliver_CancelBetweenRowsReleasesRest(t *testing.T) {
	t.Parallel()
	for _, policy := range mail.AllUncertainPolicies {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = policy })
			first := h.enqueue(t, nil)
			rest := make([]mail.Envelope, 2)
			for i := range rest {
				h.clock.advance(time.Second) // порядок Claim — по NextAttemptAt
				rest[i] = h.enqueue(t, func(m *mail.Message) { m.To.Email = fmt.Sprintf("rest%d@school.ru", i) })
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := mail.NewService(cancelOnFinish{Store: h.store, cancel: cancel}, h.tr, nil, h.cfg)
			svc.SetClock(h.clock.now)

			processed, err := svc.Deliver(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, mail.ErrUnavailable)
			assert.Equal(t, 3, processed, "исход первой строки и возврат двух")
			sent := h.tr.Sent()
			require.Len(t, sent, 1, "после отмены ушло письмо")
			assert.Equal(t, first.ID, sent[0].ID)
			for _, env := range rest {
				requireReleased(t, h.row(t, env.ID))
			}

			assert.Equal(t, 2, h.deliver(t), "остаток не ушёл следующим прогоном")
			assert.Len(t, h.tr.Sent(), 3)
			for _, env := range rest {
				row := h.row(t, env.ID)
				assert.Equal(t, mail.StatusSent, row.Status)
				assert.Equal(t, 1, row.Attempts, "остановка сожгла попытку")
			}
		})
	}
}

// ВЗЯТАЯ С Reclaimed СТРОКА НЕ ВОЗВРАЩАЕТСЯ. Её прошлая попытка не завершилась,
// и возврат стёр бы это знание: следующий Claim отдал бы её без Reclaimed, и
// UncertainPark отправил бы письмо, которое, возможно, уже ушло. Она ждёт
// аренды и уходит в failed; обычная строка той же пачки возвращается.
func TestDeliver_ReleaseKeepsReclaimedRowUnderLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = mail.UncertainPark })
	lost := h.enqueue(t, nil)
	claimed, err := h.store.Claim(context.Background(), h.clock.now(), h.cfg.Lease, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	h.clock.advance(h.cfg.Lease + time.Second)
	fresh := h.enqueue(t, func(m *mail.Message) { m.To.Email = "fresh@school.ru" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := mail.NewService(cancelOnClaim{Store: h.store, cancel: cancel}, h.tr, nil, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, 1, processed, "возвращена только обычная строка")
	requireReleased(t, h.row(t, fresh.ID))
	kept := h.row(t, lost.ID)
	assert.Equal(t, mail.StatusSending, kept.Status, "строка с Reclaimed возвращена в очередь")
	assert.Equal(t, 2, kept.Attempts)

	assert.Equal(t, 1, h.deliver(t), "возвращённая строка не ушла следующим прогоном")
	h.clock.advance(h.cfg.Lease + time.Second)
	assert.Equal(t, 1, h.deliver(t))
	row := h.row(t, lost.ID)
	assert.Equal(t, mail.StatusFailed, row.Status)
	assert.Equal(t, mail.FailUncertain, row.FailReason)
	sent := h.tr.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, fresh.ID, sent[0].ID, "письмо с неизвестным исходом отправлено снова")
}

// ОТПРАВКУ, КОТОРУЮ ОБОРВАЛА ОТМЕНА, ЯДРО НЕ ЗАПИСЫВАЕТ И НЕ ВОЗВРАЩАЕТ: письмо
// могло уйти. Записанный повтор или возврат обошёл бы UncertainPark, а
// исчерпанная попытка сожгла бы письмо — поэтому строка ждёт конца аренды, как
// при падении процесса, и её судьбу решает политика. Остаток пачки
// возвращается. MaxAttempts = 1: оборванная попытка последняя.
func TestDeliver_SendCutByCancelIsLeftToUncertainPolicy(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		policy mail.UncertainPolicy
		status mail.Status
		reason mail.FailReason
		sends  int
	}{
		"retry": {mail.UncertainRetry, mail.StatusSent, "", 3},
		"park":  {mail.UncertainPark, mail.StatusFailed, mail.FailUncertain, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) {
				c.Uncertain, c.MaxAttempts = tc.policy, 1
			})
			cut := h.enqueue(t, nil)
			h.clock.advance(time.Second) // порядок Claim — по NextAttemptAt
			next := h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sends := 0
			h.tr.SetSendHook(func(sendCtx context.Context, _ mail.Envelope) (mail.SendResult, error) {
				sends++
				if sends > 1 {
					return mail.SendResult{ProviderMessageID: "mem-2"}, nil
				}
				cancel()
				<-sendCtx.Done()
				return mail.SendResult{}, sendCtx.Err()
			})

			processed, err := h.svc.Deliver(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, mail.ErrUnavailable)
			assert.Equal(t, 1, processed, "возвращена только вторая строка")
			row := h.row(t, cut.ID)
			assert.Equal(t, mail.StatusSending, row.Status, "оборванная отправка записана или возвращена")
			assert.Equal(t, 1, row.Attempts)
			assert.Empty(t, row.LastError)
			requireReleased(t, h.row(t, next.ID))

			h.clock.advance(h.cfg.Lease + time.Second)
			assert.Equal(t, 2, h.deliver(t))
			row = h.row(t, cut.ID)
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.reason, row.FailReason)
			assert.Equal(t, mail.StatusSent, h.row(t, next.ID).Status)
			assert.Equal(t, tc.sends, sends)
		})
	}
}

// ОТМЕНА ДО ОТПРАВКИ ВОЗВРАЩАЕТ И САМУ СТРОКУ: отмена застала проверку
// стоп-листа, письмо не уходило. Ответу стоп-листа, оборванному отменой, не
// верим, и повтор с потраченной попыткой не пишем.
func TestDeliver_CancelBeforeSendReleasesRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = mail.UncertainPark })
	env := h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := mail.NewService(h.store, h.tr, cancellingSuppressor{cancel: cancel}, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)

	require.ErrorIs(t, err, context.Canceled, "прогон остановлен отменой, а не пройден")
	require.NotErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, 1, processed)
	requireReleased(t, h.row(t, env.ID))
	assert.Empty(t, h.tr.Sent(), "письмо отправлено после отмены")
}

// ПОВИСШАЯ ЗАПИСЬ ИСХОДА НЕ ПЕРЕЖИВАЕТ SendTimeout. Исход пишется мимо отмены
// прогона, и без своего срока повисшая база держала бы остановку процесса без
// предела.
func TestDeliver_HungFinishDoesNotOutliveBudget(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	h := newHarness(t, false, func(c *mail.Config) { c.SendTimeout = budget })
	h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.tr.SetSendHook(func(context.Context, mail.Envelope) (mail.SendResult, error) {
		cancel()
		return mail.SendResult{ProviderMessageID: "mem-1"}, nil
	})
	hung := &hungFinish{Store: h.store}
	svc := mail.NewService(hung, h.tr, nil, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)

	hung.requireBudget(t, budget)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded, "запись исхода оборвал не её срок")
	assert.Zero(t, processed)
}

// ПОВИСШИЙ ВОЗВРАТ ОСТАТКА ТОЖЕ НЕ ПЕРЕЖИВАЕТ SendTimeout: возврат идёт мимо
// отмены, и без срока повисшая база держала бы остановку без предела.
func TestDeliver_HungReleaseDoesNotOutliveBudget(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	h := newHarness(t, false, func(c *mail.Config) { c.SendTimeout = budget })
	h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hung := &hungFinish{Store: cancelOnClaim{Store: h.store, cancel: cancel}}
	svc := mail.NewService(hung, h.tr, nil, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)

	hung.requireBudget(t, budget)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, mail.ErrUnavailable, "сбой возврата потерян")
	require.ErrorIs(t, err, context.DeadlineExceeded, "возврат оборвал не его срок")
	assert.Zero(t, processed)
	assert.Empty(t, h.tr.Sent())
}

// requireReleased — строка возвращена в очередь: pending без аренды, попытка,
// взятая Claim, возвращена, исход не записан.
func requireReleased(t *testing.T, row mail.Envelope) {
	t.Helper()
	assert.Equal(t, mail.StatusPending, row.Status, "строка не возвращена в очередь")
	assert.Nil(t, row.LockedUntil, "аренда не снята")
	assert.Zero(t, row.Attempts, "попытка не возвращена")
	assert.Empty(t, row.LastError)
	assert.Empty(t, row.Transport)
}

// Пауза между письмами — квота провайдера (Postbox: письмо в секунду).
func TestDeliver_KeepsMinSendGap(t *testing.T) {
	t.Parallel()
	const gap = 30 * time.Millisecond
	h := newHarness(t, false, func(c *mail.Config) { c.MinSendGap = gap })
	for i := range 3 {
		h.enqueue(t, func(m *mail.Message) { m.To.Email = fmt.Sprintf("t%d@school.ru", i) })
	}

	started := time.Now()
	assert.Equal(t, 3, h.deliver(t))
	// Пауз на одну меньше, чем писем: перед первым письмом её нет.
	assert.GreaterOrEqual(t, time.Since(started), 2*gap)
}

// Два сервиса над одним хранилищем: аренда обязана не дать отправить дважды.
func TestDeliver_ConcurrentServicesSendEachRowOnce(t *testing.T) {
	t.Parallel()
	const rows = 200

	h := newHarness(t, false, nil)
	second := mail.NewService(h.store, h.tr, nil, h.cfg)
	second.SetClock(h.clock.now)
	for range rows {
		h.enqueue(t, func(m *mail.Message) { m.To.Email = uuid.NewString() + "@school.ru" })
	}

	var wg sync.WaitGroup
	for _, svc := range []*mail.Service{h.svc, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drain(t, svc)
		}()
	}
	wg.Wait()

	sent := h.tr.Sent()
	assert.Len(t, sent, rows, "каждая строка ушла ровно один раз")
	seen := make(map[uuid.UUID]bool, len(sent))
	for _, env := range sent {
		assert.False(t, seen[env.ID], "строка %s отправлена дважды", env.ID)
		seen[env.ID] = true
	}
}

// drain гоняет Deliver, пока очередь не опустеет; потолок прогонов не даёт
// тесту зависнуть, если аренда сломана.
func drain(t *testing.T, svc *mail.Service) {
	t.Helper()

	ctx := context.Background()
	for range 500 {
		if _, err := svc.Deliver(ctx); err != nil {
			t.Error(err)
			return
		}
		stats, err := svc.Stats(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		if stats.Pending == 0 {
			return
		}
	}
	t.Error("очередь не опустела за 500 прогонов")
}

// cancelOnFinish — хранилище, у которого отмена приходит сразу за записью
// исхода: между письмами, а не посреди отправки.
type cancelOnFinish struct {
	mail.Store
	cancel context.CancelFunc
}

func (s cancelOnFinish) Finish(ctx context.Context, req mail.FinishRequest) error {
	defer s.cancel()
	return s.Store.Finish(ctx, req)
}

// cancelOnClaim — хранилище, у которого отмена приходит сразу за Claim, до
// первой строки пачки.
type cancelOnClaim struct {
	mail.Store
	cancel context.CancelFunc
}

func (s cancelOnClaim) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]mail.Envelope, error) {
	defer s.cancel()
	return s.Store.Claim(ctx, now, lease, limit)
}

// cancellingSuppressor — стоп-лист, проверку в котором обрывает отмена.
type cancellingSuppressor struct{ cancel context.CancelFunc }

func (s cancellingSuppressor) IsSuppressed(ctx context.Context, _ string) (mail.Suppression, bool, error) {
	s.cancel()
	return mail.Suppression{}, false, ctx.Err()
}

func (cancellingSuppressor) Suppress(context.Context, mail.Suppression) error { return nil }

// hungFinish — хранилище, у которого любая запись через Finish (исход или
// возврат) висит, пока её не оборвёт контекст. Контекст запоминается до
// ожидания, а без срока ожидания нет вовсе: сломанный срок роняет тест
// утверждением, а не просрочкой бинаря.
type hungFinish struct {
	mail.Store

	called      bool
	enteredAt   time.Time
	entryErr    error
	deadline    time.Time
	hasDeadline bool
}

// hungCeiling — потолок ожидания hungFinish: выше бюджета теста, ниже
// просрочки бинаря.
const hungCeiling = 5 * time.Second

func (s *hungFinish) Finish(ctx context.Context, _ mail.FinishRequest) error {
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
	assert.False(t, s.deadline.After(s.enteredAt.Add(budget)), "срок записи дальше SendTimeout")
}
