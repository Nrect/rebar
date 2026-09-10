package payment_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Главная ценность сверки: человек заплатил, вебхук не доехал, и без неё он
// ждал бы товар вечно.
func TestReconcile_LostWebhook_Settles(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventSucceeded, in.AmountMinor))

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason)
	assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, in.ID).Status)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

// Сверка и вебхук дедуплицируются друг с другом: id события детерминированный,
// поэтому второй проход не делает второй строки.
func TestReconcile_AfterWebhook_NoSecondCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventSucceeded, in.AmountMinor))

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason, "терминальное намерение сверять нечего")
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

func TestReconcile_Twice_OneCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventSucceeded, in.AmountMinor))

	_, err := h.svc.Reconcile(context.Background(), in.ID)
	require.NoError(t, err)
	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonSettled, reason)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerCapture), 1)
}

// Провайдер считает платёж живым: протухать его нельзя даже за пределами TTL —
// человек оплатит списанную нами ссылку и не получит ничего.
func TestReconcile_StillPending_DoesNotExpire(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.clock.Advance(h.cfg.IntentTTL * 2)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonStillPending, reason)
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
}

// Холд, о котором провайдер говорит «холд», сверка не трогает: списать или
// снять решает потребитель, а не таймер.
func TestReconcile_HoldStaysHold(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventAuthorized, in.AmountMinor))
	h.clock.Advance(h.cfg.IntentTTL * 2)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonAuthorized, reason)
	assert.Equal(t, payment.StatusAuthorized, h.mustIntent(t, in.ID).Status)
}

// Холд, который провайдер снял сам (истёк у него), сверка увидит отменой.
func TestReconcile_ExpiredHold_IsCanceled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.hold(t)
	h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventCanceled, 0))

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCanceled, reason)
	assert.Equal(t, payment.StatusCanceled, h.mustIntent(t, in.ID).Status)
}

// Платежа у провайдера нет, и это ЗНАНИЕ, а не догадка: id мы так и не
// получили. Отсюда протухание по TTL законно.
func TestReconcile_CreatedWithoutPayment_Expires(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.unstarted(t)
	h.clock.Advance(h.cfg.IntentTTL)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonExpired, reason)
	assert.Equal(t, payment.StatusExpired, h.mustIntent(t, in.ID).Status)
}

// До TTL сверка дозавершает брошенную попытку — но только если чек не нужен:
// собрать его ей неоткуда.
func TestReconcile_UnstartedIntent_RefusesWithoutReceipt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.unstarted(t)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.ErrorIs(t, err, payment.ErrReceiptRequired)
	assert.Equal(t, payment.ReasonReceiptInvalid, reason)
	assert.Equal(t, payment.StatusCreated, h.mustIntent(t, in.ID).Status, "строку не трогаем")
}

func TestReconcile_UnstartedIntent_CompletesWhenReceiptIsNotRequired(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *payment.Config) { c.RequireReceipt = false })
	in := h.unstarted(t)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason, "это дозавершение прежней попытки, а не новая покупка")
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
}

func TestReconcile_TerminalIntent_NoOp(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.cancel(t, in)
	calls := h.prov.CallCount("GetPayment")

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonIntentClosed, reason)
	assert.Equal(t, calls, h.prov.CallCount("GetPayment"), "терминальное намерение провайдеру не показываем")
}

// Ответ про ДРУГОЙ платёж не применяется: это либо перепутанные магазины, либо
// баг адаптера, и в обоих случаях зачисление ушло бы не тому.
func TestReconcile_ProviderAnswersAboutAnotherPayment(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	foreign := h.event(in, payment.EventSucceeded, in.AmountMinor)
	foreign.ProviderPaymentID = "pay-someone-else"
	h.prov.SetPayment(in.ProviderPaymentID, foreign)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.ErrorIs(t, err, payment.ErrMalformedEvent)
	assert.Equal(t, payment.ReasonMalformedEvent, reason)
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerCapture))
}

func TestReconcile_UnknownIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	reason, err := h.svc.Reconcile(context.Background(), uuid.New())

	require.ErrorIs(t, err, payment.ErrUnknownIntent)
	assert.Equal(t, payment.ReasonUnknownIntent, reason)
}

func TestReconcile_ProviderFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())
	h.prov.SetGetErr(paymenttest.ErrProviderDown)

	reason, err := h.svc.Reconcile(context.Background(), in.ID)

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderError, reason)
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, in.ID).Status)
}

func TestStalePending_OrderAndCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	queue := h.startQueue(t, 3)
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)

	batch, err := h.svc.StalePending(context.Background(), payment.IntentCursor{}, 2)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	assert.Equal(t, queue[0].ID, batch[0].ID)
	assert.Equal(t, queue[1].ID, batch[1].ID)

	after := payment.IntentCursor{CreatedAt: batch[1].CreatedAt, ID: batch[1].ID}
	tail, err := h.svc.StalePending(context.Background(), after, 2)

	require.NoError(t, err)
	require.Len(t, tail, 1)
	assert.Equal(t, queue[2].ID, tail[0].ID, "курсор двигает очередь, а не перечитывает голову")
}

func TestStalePending_YoungIntentsAreNotStale(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.startQueue(t, 2)

	batch, err := h.svc.StalePending(context.Background(), payment.IntentCursor{}, 10)

	require.NoError(t, err)
	assert.Empty(t, batch, "только что созданное намерение зависшим не считается")
}

// Негодный лимит отбивается ДО стора, и проверяется здесь именно ВЫЗОВ, а не
// класс ошибки: стор по контракту порта отвечает на непозитивный лимит тем же
// ErrBadTransition, поэтому по ошибке гвард домена и гвард стора неотличимы, а
// смысл гварда домена ровно в том, чтобы не тратить круг в базу.
func TestStalePending_BadLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1} {
		h := newHarness(t)

		_, err := h.svc.StalePending(context.Background(), payment.IntentCursor{}, limit)

		require.ErrorIs(t, err, payment.ErrBadTransition, "лимит %d", limit)
		assert.Zero(t, h.store.CallCount("StalePending"), "лимит %d до стора не доходит", limit)
	}
}

// Счётчик зависших считается БЕЗ потолка пачки: иначе «зависших 5000»
// превратилось бы в «зависших 2» ровно тогда, когда число и есть тревога.
func TestCountStuckPending(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.startQueue(t, 3)
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)

	n, err := h.svc.CountStuckPending(context.Background())

	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

func TestCountStuckPending_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.Err = paymenttest.ErrStore

	_, err := h.svc.CountStuckPending(context.Background())

	require.ErrorIs(t, err, payment.ErrUnavailable)
}

func TestDrift(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.DriftRecords = []payment.DriftRecord{
		{IntentID: uuid.New(), Reference: "order:1", Kind: payment.DriftSucceededNoCapture},
		{IntentID: uuid.New(), Reference: "order:2", Kind: payment.DriftRefundOverCapture},
	}

	records, err := h.svc.Drift(context.Background(), 1)

	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, payment.DriftSucceededNoCapture, records[0].Kind)
}

// Тот же гвард и та же проверка, что у StalePending: «расхождений не нашлось» и
// «нас не спросили» обязаны быть разными ответами, иначе алерт с порогом 1
// молчит на пустом месте.
func TestDrift_BadLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1} {
		h := newHarness(t)

		_, err := h.svc.Drift(context.Background(), limit)

		require.ErrorIs(t, err, payment.ErrBadTransition, "лимит %d", limit)
		assert.Zero(t, h.store.CallCount("Drift"), "лимит %d до стора не доходит", limit)
	}
}

// Reconciler — задание планировщика: сигнатура Run(ctx) (int, error).
func TestReconciler_Run_WalksTheQueue(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	queue := h.startQueue(t, 3)
	for _, in := range queue {
		h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventSucceeded, in.AmountMinor))
	}
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)
	job := payment.NewReconciler(h.svc, 2)

	done, err := job.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, done)

	done, err = job.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, done, "второй прогон продолжает с курсора, а не с головы")

	for _, in := range queue {
		assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, in.ID).Status)
	}
}

// Сбой по ОДНОМУ намерению круг не останавливает, но наружу уезжает: иначе
// провайдер, молчащий про один платёж, отменил бы разбор остальных.
func TestReconciler_Run_OneFailureDoesNotStopTheLap(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	queue := h.startQueue(t, 2)
	h.prov.SetPayment(queue[1].ProviderPaymentID, h.event(queue[1], payment.EventSucceeded, testAmount))
	h.prov.SetPayment(queue[0].ProviderPaymentID, payment.Event{})
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)
	job := payment.NewReconciler(h.svc, 10)

	done, err := job.Run(context.Background())

	require.Error(t, err, "сбой обязан быть виден планировщику")
	assert.Equal(t, 1, done)
	assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, queue[1].ID).Status)
}

func TestReconciler_Run_EmptyQueueResetsCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	job := payment.NewReconciler(h.svc, 10)

	done, err := job.Run(context.Background())

	require.NoError(t, err)
	assert.Zero(t, done)
}

func TestReconciler_Run_StoreFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.Err = paymenttest.ErrStore
	job := payment.NewReconciler(h.svc, 10)

	_, err := job.Run(context.Background())

	require.ErrorIs(t, err, payment.ErrUnavailable)
}

func TestReconciler_Run_CanceledContextStops(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.startQueue(t, 2)
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)
	job := payment.NewReconciler(h.svc, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := job.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestNewReconciler_PanicsOnBadArguments(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	assert.Panics(t, func() { payment.NewReconciler(nil, 1) })
	assert.Panics(t, func() { payment.NewReconciler(h.svc, 0) })
}

// Курсор сбрасывается ТОЛЬКО на короткой пачке. Сбрось его на полной — и
// следующий прогон перечитает ту же голову очереди, а хвост не спросят никогда;
// в хвосте при этом лежит тот, кто заплатил только что и чей вебхук потерялся.
//
// Тест устроен так, что разница видна: намерения первой пачки остаются в
// pending (провайдер отвечает «платёж жив»), поэтому при сбросе курсора второй
// прогон занялся бы ими снова и до третьего не дошёл бы.
func TestReconciler_Run_FullBatchKeepsCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	queue := h.startQueue(t, 3)
	for _, in := range queue[:2] {
		h.prov.SetPayment(in.ProviderPaymentID, h.event(in, payment.EventPending, in.AmountMinor))
	}
	h.prov.SetPayment(queue[2].ProviderPaymentID, h.event(queue[2], payment.EventSucceeded, testAmount))
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)
	job := payment.NewReconciler(h.svc, 2)

	done, err := job.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, done)

	done, err = job.Run(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, done)
	assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, queue[2].ID).Status,
		"хвост очереди обязан быть разобран вторым прогоном")
}

// ХОЛД НЕ ХОРОНИТСЯ ПО НАШЕМУ TTL, и это правило, а не следствие устройства
// кода: деньги у плательщика заморожены реально, и локальный `expired` при
// живом холде — расхождение с провайдером, которое нечем починить. Уходит холд
// только списанием (Capture) или отменой у провайдера; терминальное «денег не
// будет» адаптер обязан отображать в canceled.
//
// Два рубежа, и тест проверяет оба.
func TestReconcile_HoldIsNeverExpiredByTTL(t *testing.T) {
	t.Parallel()

	// Рубеж первый: у холда всегда есть платёж у провайдера, поэтому ветка
	// протухания по TTL до него не доходит — даже когда провайдер молчит.
	t.Run("провайдер недоступен, TTL истёк", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		in := h.hold(t)
		h.prov.SetGetErr(paymenttest.ErrProviderDown)
		h.clock.Advance(h.cfg.IntentTTL * 3)

		reason, err := h.svc.Reconcile(context.Background(), in.ID)

		require.ErrorIs(t, err, payment.ErrUnavailable)
		assert.Equal(t, payment.ReasonProviderError, reason)
		assert.Equal(t, payment.StatusAuthorized, h.mustIntent(t, in.ID).Status)
	})

	// Рубеж второй: даже если строка холда каким-то образом окажется без id
	// платежа (правка мимо приложения, разъехавшийся адаптер) и сверка дойдёт до
	// протухания — таблица переходов её не пустит: authorized → expired клетки
	// нет. Проверяется именно так, а не чтением таблицы: страж, который держится
	// на «сюда всё равно не попадём», однажды перестанет держать.
	t.Run("холд без платежа у провайдера, TTL истёк", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		in := h.hold(t)
		broken := h.mustIntent(t, in.ID)
		broken.ProviderPaymentID = ""
		h.store.Seed(broken)
		h.clock.Advance(h.cfg.IntentTTL * 3)

		_, err := h.svc.Reconcile(context.Background(), in.ID)

		require.NoError(t, err)
		assert.Equal(t, payment.StatusAuthorized, h.mustIntent(t, in.ID).Status,
			"холд обязан пережить протухание: клетки authorized → expired нет")
	})
}
