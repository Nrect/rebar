package payment_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

func TestIntentByKey_FindsWhatStartCreated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req := startReq()
	in := h.start(t, req)

	got, found, err := h.svc.IntentByKey(context.Background(), req.PayerID, req.IdempotencyKey)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, in.ID, got.ID)
	require.Equal(t, in.AmountMinor, got.AmountMinor)
	require.Len(t, got.Items, len(req.Items))
}

// Ненормализованный ключ обязан найти то же намерение: иначе повтор с тем же
// ключом заводит вторую строку заказа.
func TestIntentByKey_NormalizesTheKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req := startReq()
	in := h.start(t, req)

	got, found, err := h.svc.IntentByKey(context.Background(), req.PayerID, "  "+req.IdempotencyKey+"  ")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, in.ID, got.ID)
}

func TestIntentByKey_RejectsUnusableKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, found, err := h.svc.IntentByKey(context.Background(), uuid.New(), "   ")
	require.ErrorIs(t, err, payment.ErrIdempotencyKeyInvalid)
	require.False(t, found)
	require.Zero(t, h.store.CallCount("IntentByKey"))
}

// Скоуп ключа — (payer_id, key): чужой ключ не отдаёт чужое намерение.
func TestIntentByKey_IsScopedToPayer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req := startReq()
	h.start(t, req)

	_, found, err := h.svc.IntentByKey(context.Background(), uuid.New(), req.IdempotencyKey)
	require.NoError(t, err)
	require.False(t, found)
}

func TestIntentByKey_MissingIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	got, found, err := h.svc.IntentByKey(context.Background(), uuid.New(), "never-used")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, got.ID)
}

func TestIntentByKey_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.SetErr(paymenttest.ErrStore)

	_, found, err := h.svc.IntentByKey(context.Background(), uuid.New(), "buy-1")
	require.ErrorIs(t, err, payment.ErrUnavailable)
	require.ErrorIs(t, err, paymenttest.ErrStore)
	require.False(t, found)
}

func TestIntentByID_FindsWhatStartCreated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	in := h.start(t, startReq())

	got, found, err := h.svc.IntentByID(context.Background(), in.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, in.ID, got.ID)
	require.Equal(t, payment.StatusPending, got.Status)
}

func TestIntentByID_MissingIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	got, found, err := h.svc.IntentByID(context.Background(), uuid.New())
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, got.ID)
}

func TestIntentByID_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.SetErr(paymenttest.ErrStore)

	_, found, err := h.svc.IntentByID(context.Background(), uuid.New())
	require.ErrorIs(t, err, payment.ErrUnavailable)
	require.ErrorIs(t, err, paymenttest.ErrStore)
	require.False(t, found)
}

func TestLedger_ReturnsSettledEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	in := h.sold(t)

	entries, err := h.svc.Ledger(context.Background(), in.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, payment.LedgerCapture, entries[0].Kind)
	require.Equal(t, in.AmountMinor, entries[0].AmountMinor)
}

// Пустая книга — пустой срез: у неоплаченного намерения записей не бывает, и
// это не повод отвечать ошибкой.
func TestLedger_EmptyIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	in := h.start(t, startReq())

	entries, err := h.svc.Ledger(context.Background(), in.ID)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestLedger_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.SetErr(paymenttest.ErrStore)

	entries, err := h.svc.Ledger(context.Background(), uuid.New())
	require.ErrorIs(t, err, payment.ErrUnavailable)
	require.ErrorIs(t, err, paymenttest.ErrStore)
	require.Nil(t, entries)
}
