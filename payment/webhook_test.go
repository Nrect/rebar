package payment_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// hook — сбой хука потребителя: им проверяется, что при откате не остаётся ни
// статуса, ни книги, ни строки дедупа.
var errHook = errors.New("тест: хук потребителя упал")

func webhook() payment.WebhookRequest {
	return payment.WebhookRequest{Raw: []byte(`{"event":"payment.succeeded"}`), RemoteIP: "10.0.0.1"}
}

func TestWebhook_Settles(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))

	res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason)
	assert.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
	require.NotNil(t, res.Intent.SettledAt)

	captures := h.store.EntriesOf(in.ID, payment.LedgerCapture)
	require.Len(t, captures, 1)
	assert.Equal(t, in.AmountMinor, captures[0].AmountMinor)
	assert.Nil(t, captures[0].ActorID, "автор зачисления — событие провайдера, а не человек")
}

// Повторная доставка того же события не делает второй строки в книге: провайдер
// шлёт at-least-once, и дедуп здесь не оптимизация, а условие корректности.
func TestWebhook_RepeatedEvent_OneLedgerRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	ev := h.event(in, payment.EventSucceeded, in.AmountMinor)

	h.prov.Push(ev)
	h.prov.Push(ev)
	_, first, err := h.svc.HandleWebhook(context.Background(), webhook())
	require.NoError(t, err)
	_, second, err := h.svc.HandleWebhook(context.Background(), webhook())
	require.NoError(t, err)

	assert.Equal(t, payment.ReasonSettled, first)
	assert.Equal(t, payment.ReasonDuplicateEvent, second)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
	assert.Equal(t, 2, h.store.Deliveries(ev), "счётчик доставок растёт: наш ответ не доезжает")
}

// Неподтверждённый вебхук не стоит нам ни одного соединения из пула.
func TestWebhook_BadSignature_NothingWritten(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.BadSignature = true

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.ErrorIs(t, err, payment.ErrInvalidSignature)
	assert.Equal(t, payment.ReasonSignatureInvalid, reason)
	assert.Equal(t, 0, h.store.CallCount("ApplyEvent"))
	assert.Equal(t, 0, h.store.CallCount("IntentByID"))
}

func TestWebhook_MalformedEvent_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*payment.Event){
		"без id события":  func(e *payment.Event) { e.ProviderEventID = "" },
		"чужой провайдер": func(e *payment.Event) { e.Provider = "someone_else" },
		"неизвестный тип": func(e *payment.Event) { e.Type = "almost_succeeded" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			in := h.start(t, startReq())
			ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
			mutate(&ev)
			h.prov.Push(ev)

			_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

			require.ErrorIs(t, err, payment.ErrMalformedEvent)
			assert.Equal(t, payment.ReasonMalformedEvent, reason)
			assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
		})
	}
}

// Сбой связи при подтверждении подлинности — это 503, а не 400: иначе провайдер
// посчитает вебхук доставленным и больше не придёт.
func TestWebhook_ParseUnavailable_IsNotMalformed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.ParseErr = payment.ErrUnavailable

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.ErrorIs(t, err, payment.ErrUnavailable)
	require.NotErrorIs(t, err, payment.ErrMalformedEvent)
	assert.Equal(t, payment.ReasonProviderError, reason)
}

// Орфан записывается, но ничего не зачисляет: потерянный орфан — это невидимая
// утечка ключа либо вебхук со стенда, прилетевший в прод.
func TestWebhook_UnknownIntent_RecordsEvent(t *testing.T) {
	t.Parallel()

	cases := map[string]uuid.UUID{
		"без id намерения": uuid.Nil,
		"чужое намерение":  uuid.New(),
	}

	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			ev := h.prov.Event("pay-x", id, payment.EventSucceeded, testAmount, "RUB")
			h.prov.Push(ev)

			res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

			require.NoError(t, err, "провайдеру 200: ретрай не поможет")
			assert.Equal(t, payment.ReasonUnknownIntent, reason)
			assert.Equal(t, payment.OutcomeUnknownIntent, res.Outcome)
			assert.Equal(t, 1, h.store.Deliveries(ev), "событие записано")
		})
	}
}

// Расхождение сумм не зачисляется ни в какую сторону.
func TestWebhook_AmountMismatch_NoCapture(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*payment.Event){
		"пришло меньше": func(e *payment.Event) { e.AmountMinor = testAmount - 100 },
		"пришло больше": func(e *payment.Event) { e.AmountMinor = testAmount + 100 },
		"чужая валюта":  func(e *payment.Event) { e.Currency = "KZT" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			in := h.start(t, startReq())
			ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
			mutate(&ev)
			h.prov.Push(ev)

			res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

			require.NoError(t, err, "провайдеру 200, дежурному алерт по метке")
			assert.Equal(t, payment.ReasonAmountMismatch, reason)
			assert.Equal(t, payment.OutcomeAmountMismatch, res.Outcome)
			assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
			assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
		})
	}
}

// Запоздалый pending не даунгрейдит succeeded.
func TestWebhook_LateEvent_DoesNotDowngrade(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.prov.Push(h.event(in, payment.EventPending, in.AmountMinor))

	res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonLateEvent, reason)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
}

// Та же оплата вторым событием с ДРУГИМ id: норма at-least-once, но второй
// строки в книге быть не должно.
func TestWebhook_SecondSucceededEvent_NoSecondCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
	ev.ProviderEventID += ":retry"
	h.prov.Push(ev)

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonLateEvent, reason)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

// Сумма сверяется РАНЬШЕ, чем событие признают запоздалой копией: иначе
// единственный сигнал о чужих суммах глохнет там, где деньги уже наши.
func TestWebhook_SecondSucceededEvent_OtherAmount_IsMismatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	ev := h.event(in, payment.EventSucceeded, in.AmountMinor+1)
	ev.ProviderEventID += ":retry"
	h.prov.Push(ev)

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonAmountMismatch, reason)
}

// Деньги на закрытом намерении — громкий конфликт с разбором руками.
func TestWebhook_SucceededOnClosedIntent_Conflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.cancel(t, in)
	h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))

	res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err, "провайдеру 200, человеку алерт")
	assert.Equal(t, payment.ReasonStatusConflict, reason)
	assert.Equal(t, payment.OutcomeStatusConflict, res.Outcome)
	assert.Equal(t, payment.StatusCanceled, res.Intent.Status)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
}

func TestWebhook_CancelAfterSettlement_Conflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.prov.Push(h.event(in, payment.EventCanceled, 0))

	res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonStatusConflict, reason)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
}

// Возврат, начатый в кабинете провайдера, — движение денег мимо нашей книги:
// записываем событие и оставляем человеку, а не правим книгу молча.
func TestWebhook_RefundedEvent_RecordedNotApplied(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	ev := h.event(in, payment.EventRefunded, in.AmountMinor)
	h.prov.Push(ev)

	res, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonIgnoredEvent, reason)
	assert.Equal(t, payment.OutcomeIgnored, res.Outcome)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerRefund))
	assert.Equal(t, 1, h.store.Deliveries(ev))
}

func TestWebhook_StoreFails_IsUnavailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
	h.store.Err = paymenttest.ErrStore

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonStoreError, reason)
}

// Хук потребителя — часть той же транзакции: его ошибка обязана откатить И
// статус, И книгу, И строку дедупа, иначе повтор вебхука увидит дубль и не
// применит ничего.
func TestWebhook_HookFails_RollsBackEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
	h.store.OnSettled = func(payment.Intent, payment.LedgerEntry) error { return errHook }
	h.prov.Push(ev)
	h.prov.Push(ev)

	_, reason, err := h.svc.HandleWebhook(context.Background(), webhook())
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonStoreError, reason)
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
	assert.Zero(t, h.store.Deliveries(ev), "строка дедупа откатилась вместе со всем остальным")

	h.store.OnSettled = nil
	_, reason, err = h.svc.HandleWebhook(context.Background(), webhook())

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason, "повтор того же события применяется")
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

// Хук видит состав и сумму зачисления: по ним потребитель помечает заказ
// оплаченным в той же транзакции.
func TestWebhook_HookSeesIntentAndEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	var gotItems []payment.OrderItem
	var gotAmount int64
	h.store.OnSettled = func(hooked payment.Intent, entry payment.LedgerEntry) error {
		gotItems, gotAmount = hooked.Items, entry.AmountMinor
		return nil
	}

	h.settle(t, in)

	assert.Equal(t, items(), gotItems)
	assert.Equal(t, testAmount, gotAmount)
}

// Два одновременных вебхука об одной оплате: строка зачисления одна.
func TestConcurrent_SameEvent_OneCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	ev := h.event(in, payment.EventSucceeded, in.AmountMinor)

	const workers = 6
	var wg sync.WaitGroup
	for range workers {
		h.prov.Push(ev)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = h.svc.HandleWebhook(context.Background(), webhook())
		}()
	}
	wg.Wait()

	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
	assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, in.ID).Status)
}
