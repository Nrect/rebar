package outbox_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
)

func TestEnqueue_RepeatWithSameContentIsDuplicate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	first, err := h.insert(validMessage())
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, first.Outcome)

	second, err := h.insert(validMessage())
	require.NoError(t, err, "тот же ключ и то же содержимое — успех, а не конфликт")
	assert.Equal(t, outbox.OutcomeDuplicate, second.Outcome)
	assert.Equal(t, first.Envelope.ID, second.Envelope.ID, "вернулась существующая строка")
	assert.Len(t, h.store.Rows(), 1)
}

// Тот же ключ на другое сообщение — громкий отказ, а не тихий no-op: иначе
// «начислить 100» под ключом «начислить 500» стало бы «уже сделано».
func TestEnqueue_SameKeyOtherContentIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	_, err := h.insert(validMessage())
	require.NoError(t, err)

	other := validMessage()
	other.Payload = json.RawMessage(`{"order_id":"A-42","amount_minor":50000}`)
	_, err = h.insert(other)
	require.ErrorIs(t, err, outbox.ErrKeyReused)
	assert.NotContains(t, err.Error(), "amount_minor", "payload в текст ошибки не попадает")
}

// Уникальность — парой (Kind, DedupKey): один и тот же "order:42" законен для
// разных типов сообщений.
func TestEnqueue_KeyIsUniquePerKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	_, err := h.insert(validMessage())
	require.NoError(t, err)

	receipt := validMessage()
	receipt.Kind = kindReceipt
	res, err := h.insert(receipt)
	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, res.Outcome)
	assert.Len(t, h.store.Rows(), 2)
}

// Пустой ключ дедупу не подлежит: событие без естественного ключа вставляется
// каждый раз.
func TestEnqueue_EmptyKeySkipsDedup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.DedupKey = ""
	for range 2 {
		res, err := h.insert(msg)
		require.NoError(t, err)
		assert.Equal(t, outbox.OutcomeInserted, res.Outcome)
	}
	assert.Len(t, h.store.Rows(), 2)
}

// Пустой сохранённый отпечаток повтором не считается: адаптер, потерявший
// колонку, иначе превращал бы любое сообщение в «уже в очереди».
func TestCheckDuplicate_EmptyStoredFingerprintIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	env, err := h.prod.Prepare(validMessage())
	require.NoError(t, err)

	stored := env
	stored.Fingerprint = nil
	_, err = outbox.CheckDuplicate(env, outbox.EnqueueResult{
		Outcome: outbox.OutcomeDuplicate, Envelope: stored,
	})
	require.ErrorIs(t, err, outbox.ErrKeyReused)
}

// Негодное сообщение до хранилища не доходит: валидация одна и до вставки.
func TestPrepare_InvalidMessageDoesNotReachStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.Payload = json.RawMessage(`{"a":`)
	_, err := h.insert(msg)
	require.ErrorIs(t, err, outbox.ErrInvalidMessage)
	assert.Empty(t, h.store.Rows())
}
