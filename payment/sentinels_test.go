package payment_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
	"github.com/nrect/rebar/payment/prorate"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: их ошибки — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "paymenttest")
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через errors.Is.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		err    error
		kind   errs.Kind
		prefix string
	}{
		{"ErrIdempotencyKeyInvalid", payment.ErrIdempotencyKeyInvalid, errs.KindIncorrectInput, "payment: "},
		{"ErrIdempotencyKeyReused", payment.ErrIdempotencyKeyReused, errs.KindConflict, "payment: "},
		{"ErrIdempotencyRace", payment.ErrIdempotencyRace, errs.KindConflict, "payment: "},
		{"ErrReferenceBusy", payment.ErrReferenceBusy, errs.KindConflict, "payment: "},
		{"ErrInvalidRequest", payment.ErrInvalidRequest, errs.KindIncorrectInput, "payment: "},
		{"ErrInvalidMoney", payment.ErrInvalidMoney, errs.KindIncorrectInput, "payment: "},
		{"ErrBadStatus", payment.ErrBadStatus, errs.KindUnknown, "payment: "},
		{"ErrBadTransition", payment.ErrBadTransition, errs.KindUnknown, "payment: "},
		{"ErrStatusConflict", payment.ErrStatusConflict, errs.KindConflict, "payment: "},
		{"ErrIntentClosed", payment.ErrIntentClosed, errs.KindConflict, "payment: "},
		{"ErrUnknownIntent", payment.ErrUnknownIntent, errs.KindNotFound, "payment: "},
		{"ErrAmountMismatch", payment.ErrAmountMismatch, errs.KindUnknown, "payment: "},
		{"ErrReceiptRequired", payment.ErrReceiptRequired, errs.KindUnknown, "payment: "},
		{"ErrReceiptInvalid", payment.ErrReceiptInvalid, errs.KindUnknown, "payment: "},
		{"ErrInvalidSignature", payment.ErrInvalidSignature, errs.KindIncorrectInput, "payment: "},
		{"ErrMalformedEvent", payment.ErrMalformedEvent, errs.KindIncorrectInput, "payment: "},
		{"ErrProviderRejected", payment.ErrProviderRejected, errs.KindConflict, "payment: "},
		{"ErrUnsupported", payment.ErrUnsupported, errs.KindNotImplemented, "payment: "},
		{"ErrNoActor", payment.ErrNoActor, errs.KindUnknown, "payment: "},
		{"ErrNotSettled", payment.ErrNotSettled, errs.KindConflict, "payment: "},
		{"ErrRefundTooLarge", payment.ErrRefundTooLarge, errs.KindConflict, "payment: "},
		{"ErrUnavailable", payment.ErrUnavailable, errs.KindUnavailable, "payment: "},
		{"prorate.ErrInvalidPeriod", prorate.ErrInvalidPeriod, errs.KindUnknown, "prorate: "},
		{"prorate.ErrInvalidAmount", prorate.ErrInvalidAmount, errs.KindUnknown, "prorate: "},
		{"prorate.ErrInvalidUnit", prorate.ErrInvalidUnit, errs.KindUnknown, "prorate: "},
	} {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), tc.prefix), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Сбой порта на путях из запроса, вебхука и планировщика доходит до вызывающего
// с классом 503, а не голой причиной двойника: класс несёт обёртка ядра. Хук
// потребителя — тот же путь: его зовёт стор внутри ApplyEvent и ApplyRefund, а
// их — только сервис.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("store", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())
		h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
		h.store.SetErr(errors.New("connection refused"))
		ctx := context.Background()

		_, _, err := h.svc.Start(ctx, startReq())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Start")
		_, _, err = h.svc.HandleWebhook(ctx, webhook())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "HandleWebhook")
		_, _, err = h.svc.Capture(ctx, in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Capture")
		_, _, err = h.svc.Cancel(ctx, in.ID, "cancel-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Cancel")
		_, _, err = h.svc.Refund(ctx, refundReq(in, 100, "ref-1"))
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Refund")
		_, err = h.svc.Reconcile(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Reconcile")
		_, err = h.svc.StalePending(ctx, payment.IntentCursor{}, 10)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "StalePending")
		_, err = h.svc.CountStuckPending(ctx)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "CountStuckPending")
		_, err = h.svc.Drift(ctx, 10)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Drift")
		_, _, err = h.svc.IntentByID(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "IntentByID")
		_, _, err = h.svc.IntentByKey(ctx, in.PayerID, "buy-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "IntentByKey")
		_, err = h.svc.Ledger(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Ledger")
		_, err = payment.NewReconciler(h.svc, 10).Run(ctx)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Reconciler.Run")
	})

	t.Run("provider", func(t *testing.T) {
		t.Parallel()
		for _, path := range providerPaths() {
			_, err := path.run(t, newHarness(t), paymenttest.ErrProviderDown)
			assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), path.name)
		}
		h := newHarness(t)
		h.prov.SetParseErr(paymenttest.ErrProviderDown)
		_, _, err := h.svc.HandleWebhook(context.Background(), webhook())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "HandleWebhook")
	})

	t.Run("hook", func(t *testing.T) {
		t.Parallel()
		hookDown := func(payment.Intent, payment.LedgerEntry) error { return errors.New("orders table is locked") }
		ctx := context.Background()

		h := newHarness(t)
		in := h.start(t, startReq())
		h.store.SetOnSettled(hookDown)
		h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
		_, _, err := h.svc.HandleWebhook(ctx, webhook())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "OnSettled из HandleWebhook")

		h = newHarness(t)
		held := h.hold(t)
		h.store.SetOnSettled(hookDown)
		_, _, err = h.svc.Capture(ctx, held.ID, held.AmountMinor, receiptFor(testAmount), "cap-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "OnSettled из Capture")

		h = newHarness(t)
		sold := h.sold(t)
		h.store.SetOnRefunded(hookDown)
		_, _, err = h.svc.Refund(ctx, refundReq(sold, 100, "ref-1"))
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "OnRefunded из Refund")
	})
}

// Возврат по негодной книге — инцидент, а не «не оплачено» и не «вы ошиблись»:
// класс 503 перекрывает 409 у ErrNotSettled и 400 у ErrInvalidMoney, а
// errors.Is и Reason остаются прежними. Настоящее «не оплачено» — 409.
func TestRefund_BrokenLedgerIsUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("не оплачено", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())

		_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 100, "ref-1"))

		require.ErrorIs(t, err, payment.ErrNotSettled)
		assert.Equal(t, payment.ReasonNotSettled, reason)
		assert.Equal(t, errs.KindConflict, errs.KindOf(err))
	})

	t.Run("оплачено без записи зачисления", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.sold(t)
		h.store.ClearEntries()

		_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 100, "ref-1"))

		require.ErrorIs(t, err, payment.ErrNotSettled)
		assert.Equal(t, payment.ReasonNotSettled, reason)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	})

	t.Run("запись в чужой валюте", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.sold(t)
		h.store.SeedEntry(payment.LedgerEntry{
			ID: uuid.New(), IntentID: in.ID, Kind: payment.LedgerRefund, AmountMinor: 100, Currency: "KZT",
		})

		_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 100, "ref-1"))

		require.ErrorIs(t, err, payment.ErrInvalidMoney)
		assert.Equal(t, payment.ReasonStoreError, reason)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	})
}
