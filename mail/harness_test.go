package mail_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

var baseTime = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// testClock — управляемые часы. Под замком, потому что в тесте на гонку их
// читают горутины Deliver.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// harness — сервис над двойниками с управляемыми часами.
type harness struct {
	svc   *mail.Service
	store *mailtest.MemStore
	tr    *mailtest.Transport
	supp  *mailtest.MemSuppressor
	clock *testClock
	cfg   mail.Config
}

// newHarness собирает сервис на двойниках. Стоп-лист подключается по флагу:
// nil-Suppressor — конфигурация первого прода (ADR-0001, «Стоп-лист»).
func newHarness(t *testing.T, withSupp bool, tune func(*mail.Config)) *harness {
	t.Helper()

	cfg := validConfig()
	// Пауза между письмами замедлила бы каждый тест; она проверяется отдельно.
	cfg.MinSendGap = 0
	if tune != nil {
		tune(&cfg)
	}
	h := &harness{
		store: mailtest.NewMemStore(),
		tr:    mailtest.NewTransport(),
		clock: &testClock{at: baseTime},
		cfg:   cfg,
	}
	var supp mail.Suppressor // именно так: h.supp в интерфейсе дал бы не-nil
	if withSupp {
		h.supp = mailtest.NewMemSuppressor()
		supp = h.supp
	}
	h.svc = mail.NewService(h.store, h.tr, supp, cfg)
	h.svc.SetClock(h.clock.now)
	return h
}

// enqueue кладёт письмо с уникальным ключом и возвращает вставленную строку.
func (h *harness) enqueue(t *testing.T, mutate func(*mail.Message)) mail.Envelope {
	t.Helper()

	msg := validMessage()
	msg.DedupKey = "verify:" + uuid.NewString()
	if mutate != nil {
		mutate(&msg)
	}
	res, err := h.svc.Enqueue(context.Background(), msg)
	require.NoError(t, err)
	require.Equal(t, mail.OutcomeInserted, res.Outcome)
	return res.Envelope
}

// row — строка хранилища по ID.
func (h *harness) row(t *testing.T, id uuid.UUID) mail.Envelope {
	t.Helper()

	row, ok := h.store.Get(id)
	require.True(t, ok, "строка %s пропала из хранилища", id)
	return row
}

// deliver — один прогон; ошибка прогона считается провалом теста.
func (h *harness) deliver(t *testing.T) int {
	t.Helper()

	processed, err := h.svc.Deliver(context.Background())
	require.NoError(t, err)
	return processed
}
