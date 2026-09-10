package paymenttest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
)

// Хуки зовутся ПОД ЗАМКОМ двойника — как у адаптера внутри транзакции: отпусти
// он замок, параллельный вызов вклинился бы между предикатом и применением, и
// двойник стал бы мягче базы. Отсюда контракт «хук двойник не трогает»; здесь
// он закреплён без таймаутов — пробой замка изнутри хука обязан не пройти.
func TestMemStore_HooksRunUnderTheLock(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	var settledHeld, refundedHeld bool
	store.SetOnSettled(func(payment.Intent, payment.LedgerEntry) error {
		settledHeld = !store.mu.TryLock()
		if !settledHeld {
			store.mu.Unlock()
		}
		return nil
	})
	store.SetOnRefunded(func(payment.Intent, payment.LedgerEntry) error {
		refundedHeld = !store.mu.TryLock()
		if !refundedHeld {
			store.mu.Unlock()
		}
		return nil
	})

	in := suitePending(t, store)
	ev := suiteEvent(in, payment.EventSucceeded)
	capture := suiteCapture(in, ev, suiteNow())
	settled, err := store.ApplyEvent(t.Context(),
		suiteApplyRequest(in, ev, payment.StatusSucceeded, capture, suiteNow()))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, settled.Outcome)

	refunded, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID,
		Refund: suiteRefund(in, *capture, 100, "refund-1", suiteNow()), Now: suiteNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, refunded.Outcome)

	assert.True(t, settledHeld, "OnSettled зовётся под замком двойника")
	assert.True(t, refundedHeld, "OnRefunded зовётся под замком двойника")
}
