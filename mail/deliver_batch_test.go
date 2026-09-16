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
// отмены: строка sent, прогон после конца аренды её не шлёт. Раньше Finish шёл
// по отменённому контексту и до базы не доезжал — строка ждала аренды, и при
// UncertainRetry письмо уезжало второй раз, а при UncertainPark строка уходила
// в failed, хотя письмо доставлено. Судьбу второй, не тронутой строки решает
// политика: при UncertainPark она уходит в failed неотправленной.
func TestDeliver_CancelDuringSendRecordsOutcome(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		policy   mail.UncertainPolicy
		restSent bool
	}{
		"retry": {mail.UncertainRetry, true},
		"park":  {mail.UncertainPark, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = tc.policy })
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
			assert.Equal(t, 1, processed)
			row := h.row(t, first.ID)
			assert.Equal(t, mail.StatusSent, row.Status, "исход отправленного письма не записан")
			assert.Equal(t, "mem-"+first.ID.String(), row.ProviderMessageID)

			h.clock.advance(h.cfg.Lease + time.Second)
			assert.Equal(t, 1, h.deliver(t), "после конца аренды взята не одна вторая строка")
			assert.Equal(t, mail.StatusSent, h.row(t, first.ID).Status)
			want := []uuid.UUID{first.ID}
			if tc.restSent {
				want = append(want, second.ID)
			}
			assert.Equal(t, want, sent, "отправленное письмо ушло второй раз")
		})
	}
}

// ОТМЕНА МЕЖДУ ПИСЬМАМИ НЕ ТРОГАЕТ ОСТАТОК ПАЧКИ: её ловит waitTurn до
// отправки. Вторая строка остаётся такой, какой её взял Claim, — не отправлена,
// исход не записан — и ждёт конца аренды.
func TestDeliver_CancelBetweenRowsLeavesRestUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	first := h.enqueue(t, nil)
	h.clock.advance(time.Second) // порядок Claim — по NextAttemptAt
	second := h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := mail.NewService(cancelOnFinish{Store: h.store, cancel: cancel}, h.tr, nil, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, 1, processed)
	sent := h.tr.Sent()
	require.Len(t, sent, 1, "после отмены ушло письмо")
	assert.Equal(t, first.ID, sent[0].ID)
	assert.Equal(t, mail.StatusSent, h.row(t, first.ID).Status)

	rest := h.row(t, second.ID)
	assert.Equal(t, mail.StatusSending, rest.Status, "остаток пачки не ждёт конца аренды")
	assert.Equal(t, 1, rest.Attempts)
	assert.Empty(t, rest.Transport, "исход строки после отмены записан")
	assert.Empty(t, rest.LastError)
}

// ОТПРАВКУ, КОТОРУЮ ОБОРВАЛА ОТМЕНА, ЯДРО НЕ ЗАПИСЫВАЕТ: письмо могло уйти.
// Записанный повтор обошёл бы UncertainPark, а исчерпанная попытка сожгла бы
// письмо — поэтому строка ждёт конца аренды, как при падении процесса, и её
// судьбу решает политика. MaxAttempts = 1: оборванная попытка последняя.
func TestDeliver_SendCutByCancelIsLeftToUncertainPolicy(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		policy mail.UncertainPolicy
		status mail.Status
		reason mail.FailReason
		sends  int
	}{
		"retry": {mail.UncertainRetry, mail.StatusSent, "", 2},
		"park":  {mail.UncertainPark, mail.StatusFailed, mail.FailUncertain, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, false, func(c *mail.Config) {
				c.Uncertain, c.MaxAttempts = tc.policy, 1
			})
			env := h.enqueue(t, nil)

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
			assert.Zero(t, processed)
			row := h.row(t, env.ID)
			assert.Equal(t, mail.StatusSending, row.Status, "исход оборванной отправки записан")
			assert.Empty(t, row.LastError)

			h.clock.advance(h.cfg.Lease + time.Second)
			assert.Equal(t, 1, h.deliver(t))
			row = h.row(t, env.ID)
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.reason, row.FailReason)
			assert.Equal(t, tc.sends, sends)
		})
	}
}

// ОТМЕНА ПОСРЕДИ ПРОВЕРКИ СТОП-ЛИСТА ЗАПИСЫВАЕТ ПОВТОР: письмо не уходило, и
// исход «не проверили — не шлём» известен. Без записи строка ждала бы аренды,
// и при UncertainPark неотправленное письмо ушло бы в failed.
func TestDeliver_CancelDuringSuppressorCheckRecordsRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.Uncertain = mail.UncertainPark })
	env := h.enqueue(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := mail.NewService(h.store, h.tr, cancellingSuppressor{cancel: cancel}, h.cfg)
	svc.SetClock(h.clock.now)

	processed, err := svc.Deliver(ctx)

	require.NoError(t, err, "строка в пачке одна, и её исход записан")
	assert.Equal(t, 1, processed)
	row := h.row(t, env.ID)
	assert.Equal(t, mail.StatusPending, row.Status, "повтор не записан")
	assert.Contains(t, row.LastError, context.Canceled.Error())
	assert.Empty(t, h.tr.Sent())
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

	require.True(t, hung.called, "до записи исхода прогон не дошёл")
	require.NotErrorIs(t, hung.entryErr, context.Canceled, "запись исхода получила отменённый контекст прогона")
	require.True(t, hung.hasDeadline, "у записи исхода нет срока")
	assert.False(t, hung.deadline.After(hung.enteredAt.Add(budget)), "срок записи исхода дальше SendTimeout")
	require.ErrorIs(t, err, mail.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded, "запись исхода оборвал не её срок")
	assert.Zero(t, processed)
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

// cancellingSuppressor — стоп-лист, проверку в котором обрывает отмена.
type cancellingSuppressor struct{ cancel context.CancelFunc }

func (s cancellingSuppressor) IsSuppressed(ctx context.Context, _ string) (mail.Suppression, bool, error) {
	s.cancel()
	return mail.Suppression{}, false, ctx.Err()
}

func (cancellingSuppressor) Suppress(context.Context, mail.Suppression) error { return nil }

// hungFinish — хранилище, у которого запись исхода висит, пока её не оборвёт
// контекст. Контекст запоминается до ожидания, а без срока ожидания нет вовсе:
// сломанный срок роняет тест утверждением, а не просрочкой бинаря.
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
		return errors.New("hungFinish: у записи исхода нет срока")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hungCeiling):
		return errors.New("hungFinish: срок записи исхода не сработал")
	}
}
