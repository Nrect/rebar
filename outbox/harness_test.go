package outbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

var baseTime = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// kindLegacy объявлен в Config, но не зарегистрирован ни одним хендлером: так
// проверяется, что такие строки не забираются и видны в Stats.Unhandled.
const (
	kindPaid    outbox.Kind = "order.paid"
	kindReceipt outbox.Kind = "receipt.send"
	kindLegacy  outbox.Kind = "legacy.event"
)

func validConfig() outbox.Config {
	return outbox.Config{
		Kinds:           []outbox.Kind{kindPaid, kindReceipt, kindLegacy},
		MaxAttempts:     8,
		Backoff:         outbox.Backoff{Base: 30 * time.Second, Max: time.Hour},
		Lease:           2 * time.Minute,
		HandlerTimeout:  30 * time.Second,
		BatchSize:       20,
		Retention:       7 * 24 * time.Hour,
		MaxPayloadBytes: 256 << 10,
	}
}

func validMessage() outbox.Message {
	return outbox.Message{
		Kind:          kindPaid,
		Payload:       json.RawMessage(`{"order_id":"A-42","amount_minor":12900}`),
		DedupKey:      "order.paid:A-42",
		AggregateType: "order",
		AggregateID:   "A-42",
		SchemaVersion: 1,
		Headers:       map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	}
}

// harness — продюсер и воркер над двойниками с управляемыми часами.
type harness struct {
	prod    *outbox.Producer
	worker  *outbox.Worker
	store   *outboxtest.MemStore
	handler *outboxtest.RecordingHandler
	reg     *outbox.Registry
	clock   *outboxtest.Clock
	cfg     outbox.Config
}

func newHarness(t *testing.T, tune func(*outbox.Config)) *harness {
	t.Helper()

	cfg := validConfig()
	if tune != nil {
		tune(&cfg)
	}
	h := &harness{
		store:   outboxtest.NewMemStore(),
		handler: outboxtest.NewRecordingHandler(),
		reg:     outbox.NewRegistry(),
		clock:   outboxtest.NewClock(baseTime),
		cfg:     cfg,
	}
	h.reg.Register(kindPaid, h.handler)
	h.reg.Register(kindReceipt, h.handler)

	h.prod = outbox.NewProducer(h.store, cfg)
	h.prod.SetClock(h.clock.Now)

	worker, err := outbox.NewWorker(h.store, h.reg, cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)
	h.worker = worker
	return h
}

// enqueue кладёт сообщение с уникальным ключом и возвращает вставленную строку:
// Prepare, вставка двойником (в проде — адаптером в транзакции факта) и
// проверка законности повтора.
func (h *harness) enqueue(t *testing.T, mutate func(*outbox.Message)) outbox.Envelope {
	t.Helper()

	msg := validMessage()
	msg.DedupKey = "order.paid:" + uuid.NewString()
	if mutate != nil {
		mutate(&msg)
	}
	res, err := h.insert(msg)
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	return res.Envelope
}

// insert — полный путь вставки без require: им пользуются и тесты на ошибки.
func (h *harness) insert(msg outbox.Message) (outbox.EnqueueResult, error) {
	env, err := h.prod.Prepare(msg)
	if err != nil {
		return outbox.EnqueueResult{}, err
	}
	res, err := h.store.Enqueue(context.Background(), env)
	if err != nil {
		return outbox.EnqueueResult{}, err
	}
	return outbox.CheckDuplicate(env, res)
}

// row — строка хранилища по ID.
func (h *harness) row(t *testing.T, id uuid.UUID) outbox.Envelope {
	t.Helper()

	row, ok := h.store.Get(id)
	require.True(t, ok, "строка %s пропала из хранилища", id)
	return row
}

// drain — один прогон; ошибка прогона считается провалом теста.
func (h *harness) drain(t *testing.T) int {
	t.Helper()

	processed, err := h.worker.Drain(context.Background())
	require.NoError(t, err)
	return processed
}
