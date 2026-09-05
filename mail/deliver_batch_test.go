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
	h.store.FinishErr = errors.New("connection reset")

	processed, err := h.svc.Deliver(context.Background())
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, 0, processed)
	assert.Len(t, h.tr.Sent(), 1, "вторая строка не отправлена")
}

// Отмена ctx останавливает пачку; взятые строки дождутся истечения аренды.
func TestDeliver_ContextCancelStopsBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.enqueue(t, nil)
	h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sends := 0
	h.tr.SendHook = func(context.Context, mail.Envelope) (mail.SendResult, error) {
		sends++
		cancel()
		return mail.SendResult{ProviderMessageID: "mem-1"}, nil
	}

	processed, err := h.svc.Deliver(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, processed, "исход первой строки записан")
	assert.Equal(t, 1, sends)

	// Порядок Claim задаёт хранилище; какая именно строка осталась — не важно.
	statuses := map[mail.Status]int{}
	for _, row := range h.store.Rows() {
		statuses[row.Status]++
	}
	assert.Equal(t, map[mail.Status]int{mail.StatusSent: 1, mail.StatusSending: 1}, statuses,
		"взятая строка ждёт истечения аренды")
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
