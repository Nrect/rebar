package payment_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

func TestCapture_AuthorizedToSucceeded(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)

	got, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason)
	assert.Equal(t, payment.StatusSucceeded, got.Status)
	captures := h.store.EntriesOf(in.ID, payment.LedgerCapture)
	require.Len(t, captures, 1)
	assert.Equal(t, in.AmountMinor, captures[0].AmountMinor)

	require.Len(t, h.prov.Captures(), 1)
	assert.Equal(t, "shop:capture:"+in.ID.String(), h.prov.Captures()[0].IdempotencyKey,
		"ключ провайдеру производный: повтор не спишет холд дважды")
	assert.NotNil(t, h.prov.Captures()[0].Receipt, "расчёт происходит в момент списания — чек уезжает сюда")
}

// Вебхук о том же списании приезжает следом и не делает второй строки: id
// события у ответа на Capture и у вебхука один и тот же.
func TestCapture_ThenWebhook_OneLedgerRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	_, _, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
	require.NoError(t, err)

	h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonDuplicateEvent, reason)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

// Адаптер без двухстадийной оплаты отвечает ErrUnsupported: это отказ по
// конструкции, а не сбой, и ретраить его бессмысленно.
func TestCapture_UnsupportedProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	h.prov.SetNoHolds(true)

	_, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")

	require.ErrorIs(t, err, payment.ErrUnsupported)
	assert.Equal(t, payment.ReasonUnsupported, reason)
	assert.Equal(t, payment.StatusAuthorized, h.mustIntent(t, in.ID).Status)
}

// Частичного списания нет: сумма заморожена в намерении.
func TestCapture_OtherAmount_Refused(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{"меньше холда": testAmount - 100, "больше холда": testAmount + 100}

	for name, amount := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			in := h.hold(t)

			_, reason, err := h.svc.Capture(context.Background(), in.ID, amount, receiptFor(amount), "cap-1")

			require.ErrorIs(t, err, payment.ErrAmountMismatch)
			assert.Equal(t, payment.ReasonAmountMismatch, reason)
			assert.Equal(t, 0, h.prov.CallCount("Capture"))
		})
	}
}

func TestCapture_NotAuthorized_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())

	_, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")

	require.ErrorIs(t, err, payment.ErrStatusConflict)
	assert.Equal(t, payment.ReasonStatusConflict, reason)
	assert.Equal(t, 0, h.prov.CallCount("Capture"))
}

// Повтор после потерянного ответа обязан вернуть то же, что и первый вызов.
func TestCapture_AlreadyCaptured_IsReplay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	_, _, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
	require.NoError(t, err)

	got, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, payment.StatusSucceeded, got.Status)
	assert.Equal(t, 1, h.prov.CallCount("Capture"), "второй раз к провайдеру не ходим")
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

func TestCapture_NoReceipt_RefusedBeforeProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)

	_, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, nil, "cap-1")

	require.ErrorIs(t, err, payment.ErrReceiptRequired)
	assert.Equal(t, payment.ReasonReceiptInvalid, reason)
	assert.Equal(t, 0, h.prov.CallCount("Capture"))
}

func TestCapture_BadKeyAndUnknownIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)

	_, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), " ")
	require.ErrorIs(t, err, payment.ErrIdempotencyKeyInvalid)
	assert.Equal(t, payment.ReasonKeyInvalid, reason)

	_, reason, err = h.svc.Capture(context.Background(), uuid.New(), testAmount, receiptFor(testAmount), "cap-1")
	require.ErrorIs(t, err, payment.ErrUnknownIntent)
	assert.Equal(t, payment.ReasonUnknownIntent, reason)
}

func TestCapture_ProviderFails_HoldStays(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	h.prov.SetCaptureErr(paymenttest.ErrProviderDown)

	_, reason, err := h.svc.Capture(context.Background(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderError, reason)
	assert.Equal(t, payment.StatusAuthorized, h.mustIntent(t, in.ID).Status)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
}

func TestCancel_AuthorizedToCanceled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)

	got, reason, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCanceled, reason)
	assert.Equal(t, payment.StatusCanceled, got.Status)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), "снятый холд денег не двигает")
}

func TestCancel_PendingIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())

	got, reason, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCanceled, reason)
	assert.Equal(t, payment.StatusCanceled, got.Status)
}

// У намерения в created платежа у провайдера может ещё не быть, а может и быть —
// ответ потерялся. Закрыть такую строку значило бы открыть окно, в котором на
// отменённое намерение приходят деньги.
func TestCancel_BeforeProviderKnows_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.SetCreateErr(paymenttest.ErrProviderDown)
	res, _, err := h.svc.Start(context.Background(), startReq())
	require.Error(t, err)
	require.Equal(t, payment.StatusCreated, res.Intent.Status)

	_, reason, err := h.svc.Cancel(context.Background(), res.Intent.ID, "cancel-1")

	require.ErrorIs(t, err, payment.ErrInvalidRequest)
	assert.Equal(t, payment.ReasonInvalidRequest, reason)
	assert.Equal(t, 0, h.prov.CallCount("Cancel"))
}

func TestCancel_Terminal_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	_, reason, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")

	require.ErrorIs(t, err, payment.ErrIntentClosed)
	assert.Equal(t, payment.ReasonIntentClosed, reason)
	assert.Equal(t, 0, h.prov.CallCount("Cancel"))
}

func TestCancel_AlreadyCanceled_IsReplay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	_, _, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")
	require.NoError(t, err)

	got, reason, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, payment.StatusCanceled, got.Status)
	assert.Equal(t, 1, h.prov.CallCount("Cancel"))
}

func TestCancel_UnsupportedProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	h.prov.SetNoHolds(true)

	_, reason, err := h.svc.Cancel(context.Background(), in.ID, "cancel-1")

	require.ErrorIs(t, err, payment.ErrUnsupported)
	assert.Equal(t, payment.ReasonUnsupported, reason)
}
