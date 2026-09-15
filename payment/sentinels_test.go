package payment_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
// (ADR-0007). Двойники в allow: своего класса у их sentinel нет — класс
// приходит обёрткой (ADR-0007, «Двойники»).
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
		{"ErrInvalidRequest", payment.ErrInvalidRequest, errs.KindUnknown, "payment: "},
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
// с классом 503: класс несёт обёртка ядра. Хранилище в подтесте store — голая
// заглушка: paymenttest.MemStore заворачивает сбой сам, как paymentpg, и
// снятой обёртки ядра страж бы не увидел. Сбой провайдера и хука двойники
// отдают голым. Хук потребителя — тот же путь: его зовёт стор внутри
// ApplyEvent и ApplyRefund, а их — только сервис.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	// Состояние готовит стенд на двойнике; падающие вызовы идут в сервис над
	// заглушкой с тем же провайдером и наблюдателем.
	t.Run("store", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())
		h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
		svc := payment.NewService(bareStore{err: errors.New("connection refused")}, h.prov, h.obs, h.cfg)
		svc.SetClock(h.clock.Now)
		ctx := context.Background()

		_, _, err := svc.Start(ctx, startReq())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Start")
		_, _, err = svc.HandleWebhook(ctx, webhook())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "HandleWebhook")
		_, _, err = svc.Capture(ctx, in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Capture")
		_, _, err = svc.Cancel(ctx, in.ID, "cancel-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Cancel")
		_, _, err = svc.Refund(ctx, refundReq(in, 100, "ref-1"))
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Refund")
		_, err = svc.Reconcile(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Reconcile")
		_, err = svc.StalePending(ctx, payment.IntentCursor{}, 10)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "StalePending")
		_, err = svc.CountStuckPending(ctx)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "CountStuckPending")
		_, err = svc.Drift(ctx, 10)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Drift")
		_, _, err = svc.IntentByID(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "IntentByID")
		_, _, err = svc.IntentByKey(ctx, in.PayerID, "buy-1")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "IntentByKey")
		_, err = svc.Ledger(ctx, in.ID)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Ledger")
		_, err = payment.NewReconciler(svc, 10).Run(ctx)
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

// bareStore — payment.Store, отдающий сбой голым на каждом методе.
type bareStore struct{ err error }

func (s bareStore) CreateIntent(context.Context, payment.Intent) error { return s.err }

func (s bareStore) IntentByKey(context.Context, uuid.UUID, string) (payment.Intent, bool, error) {
	return payment.Intent{}, false, s.err
}

func (s bareStore) IntentByID(context.Context, uuid.UUID) (payment.Intent, bool, error) {
	return payment.Intent{}, false, s.err
}

func (s bareStore) Transition(context.Context, payment.TransitionRequest) (payment.TransitionResult, error) {
	return payment.TransitionResult{}, s.err
}

func (s bareStore) ApplyEvent(context.Context, payment.ApplyEventRequest) (payment.ApplyEventResult, error) {
	return payment.ApplyEventResult{}, s.err
}

func (s bareStore) ApplyRefund(context.Context, payment.ApplyRefundRequest) (payment.ApplyRefundResult, error) {
	return payment.ApplyRefundResult{}, s.err
}

func (s bareStore) Ledger(context.Context, uuid.UUID) ([]payment.LedgerEntry, error) {
	return nil, s.err
}

func (s bareStore) StalePending(context.Context, time.Time, payment.IntentCursor, int) ([]payment.Intent, error) {
	return nil, s.err
}

func (s bareStore) CountStuckPending(context.Context, time.Time) (int64, error) { return 0, s.err }

func (s bareStore) Drift(context.Context, time.Time, int) ([]payment.DriftRecord, error) {
	return nil, s.err
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

// Непригодный ответ провайдера на НАШ вызов — его аномалия, 503, как мусорный
// ответ CreatePayment; тот же изъян в теле вебхука — 400. errors.Is и Reason у
// обоих прежние.
func TestProviderAnswerAnomalyIsUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("ответ сверки про чужой платёж", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())
		foreign := h.event(in, payment.EventSucceeded, in.AmountMinor)
		foreign.ProviderPaymentID = "pay-someone-else"
		h.prov.SetPayment(in.ProviderPaymentID, foreign)

		reason, err := h.svc.Reconcile(context.Background(), in.ID)

		require.ErrorIs(t, err, payment.ErrMalformedEvent)
		assert.Equal(t, payment.ReasonMalformedEvent, reason)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	})

	t.Run("ответ сверки без id события", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())
		ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
		ev.ProviderEventID = ""
		h.prov.SetPayment(in.ProviderPaymentID, ev)

		reason, err := h.svc.Reconcile(context.Background(), in.ID)

		require.ErrorIs(t, err, payment.ErrMalformedEvent)
		assert.Equal(t, payment.ReasonMalformedEvent, reason)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	})

	t.Run("вебхук без id события — 400", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		in := h.start(t, startReq())
		ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
		ev.ProviderEventID = ""
		h.prov.Push(ev)

		_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

		require.ErrorIs(t, err, payment.ErrMalformedEvent)
		assert.Equal(t, payment.ReasonMalformedEvent, reason)
		assert.Equal(t, errs.KindIncorrectInput, errs.KindOf(err))
	})
}

// Эхо возврата с негодной суммой — аномалия провайдера: у ErrAmountMismatch
// класса нет, и класс 400 причины сквозь неё не проступает.
func TestRefund_ProviderEchoWithBadMoneyHasNoClientKind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.prov.SetRefundEcho(-1)

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrAmountMismatch)
	assert.Equal(t, payment.ReasonAmountMismatch, reason)
	assert.Equal(t, errs.KindUnknown, errs.KindOf(err))
}
