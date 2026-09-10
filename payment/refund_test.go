package payment_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

func refundReq(in payment.Intent, amount int64, key string) payment.RefundRequest {
	return payment.RefundRequest{
		IntentID:       in.ID,
		AmountMinor:    amount,
		IdempotencyKey: key,
		ActorID:        uuid.New(),
		Receipt:        receiptFor(amount),
	}
}

func TestRefund_ReversesCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	res, reason, err := h.svc.Refund(context.Background(), refundReq(in, testAmount, "ref-1"))

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonRefunded, reason)
	assert.Equal(t, testAmount, res.Entry.AmountMinor)
	assert.True(t, res.Net.IsZero(), "полный возврат обнуляет нетто")
	require.NotNil(t, res.Entry.ActorID, "денежная строка без автора не отвечает, кто вернул деньги")
	require.NotNil(t, res.Entry.ReversesEntryID)
	assert.Equal(t, h.store.EntriesOf(in.ID, payment.LedgerCapture)[0].ID, *res.Entry.ReversesEntryID)
	// Статус возвратом не меняется: succeeded терминален, а refunded статусом не
	// является — иначе запоздалое succeeded воскресило бы оплату после возврата.
	assert.Equal(t, payment.StatusSucceeded, h.mustIntent(t, in.ID).Status)
}

// Частичные возвраты складываются до потолка зачисления, а не до одной записи.
func TestRefund_PartialsAddUpToCapture(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	first, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))
	require.NoError(t, err)
	assert.Equal(t, payment.ReasonRefunded, reason)
	assert.Equal(t, int64(69800), first.Net.Minor())

	second, reason, err := h.svc.Refund(context.Background(), refundReq(in, 69800, "ref-2"))
	require.NoError(t, err)
	assert.Equal(t, payment.ReasonRefunded, reason)
	assert.True(t, second.Net.IsZero())

	// Третий сверх зачисления отбит — и отбит ДО похода к провайдеру.
	_, reason, err = h.svc.Refund(context.Background(), refundReq(in, 1, "ref-3"))

	require.ErrorIs(t, err, payment.ErrRefundTooLarge)
	assert.Equal(t, payment.ReasonRefundTooLarge, reason)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerRefund), 2)
	assert.Equal(t, 2, h.prov.CallCount("Refund"))
}

func TestRefund_OverCapture_RefusedBeforeProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, testAmount+1, "ref-1"))

	require.ErrorIs(t, err, payment.ErrRefundTooLarge)
	assert.Equal(t, payment.ReasonRefundTooLarge, reason)
	assert.Equal(t, 0, h.prov.CallCount("Refund"))
}

func TestRefund_ZeroAmount_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	_, _, err := h.svc.Refund(context.Background(), refundReq(in, 0, "ref-1"))

	require.ErrorIs(t, err, payment.ErrInvalidMoney)
	assert.Equal(t, 0, h.prov.CallCount("Refund"))
}

// Тот же ключ с той же суммой — законный повтор и УСПЕХ: вызывающий, потерявший
// ответ, не должен двигать деньги второй раз.
func TestRefund_SameKey_IsReplay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	req := refundReq(in, 50000, "ref-1")
	first, _, err := h.svc.Refund(context.Background(), req)
	require.NoError(t, err)

	second, reason, err := h.svc.Refund(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, first.Entry.ID, second.Entry.ID)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerRefund), 1)
	assert.Equal(t, 1, h.prov.CallCount("Refund"))
}

// Тот же ключ на ДРУГУЮ сумму — 409: под одним ключом два разных возврата не
// проводятся.
func TestRefund_SameKey_OtherAmount_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	_, _, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))
	require.NoError(t, err)

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 60000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrIdempotencyKeyReused)
	assert.Equal(t, payment.ReasonKeyReused, reason)
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerRefund), 1)
}

// Ключ провайдеру производный и РАЗНЫЙ у разных частичных возвратов: один на
// все попытки вернуть эти деньги, но не один на два разных возврата.
func TestRefund_ProviderKeysAreDerivedAndDistinct(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	_, _, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))
	require.NoError(t, err)
	_, _, err = h.svc.Refund(context.Background(), refundReq(in, 20000, "ref-2"))
	require.NoError(t, err)

	keys := h.prov.RefundKeys()
	require.Len(t, keys, 2)
	for _, key := range keys {
		assert.Contains(t, key, "shop:refund:"+in.ID.String())
		assert.NotContains(t, key, "ref-", "клиентский ключ уезжает провайдеру только хешем")
	}
}

// Повтор после сбоя стора с ТЕМ ЖЕ клиентским ключом не двигает деньги второй
// раз: ключ провайдера от него производный.
func TestRefund_RetryAfterStoreFailure_OneRefundAtProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	req := refundReq(in, 50000, "ref-1")
	h.store.Err = paymenttest.ErrStore
	_, reason, err := h.svc.Refund(context.Background(), req)
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonStoreError, reason)

	h.store.Err = nil
	_, reason, err = h.svc.Refund(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonRefunded, reason)
	assert.Len(t, h.prov.RefundKeys(), 1, "провайдер увидел ОДИН возврат, а не два")
	assert.Len(t, h.store.EntriesOf(in.ID, payment.LedgerRefund), 1)
}

// Провайдер вернул не ту сумму: ни одна из двух цифр не годится — наша не
// соответствует деньгам, его не соответствует уже пробитому чеку.
func TestRefund_ProviderEchoesOtherAmount_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.prov.SetRefundEcho(999)

	res, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrAmountMismatch)
	assert.Equal(t, payment.ReasonAmountMismatch, reason)
	assert.Zero(t, res.Entry.ID)
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerRefund), "строка не записана")
}

func TestRefund_NoActor_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	req := refundReq(in, 50000, "ref-1")
	req.ActorID = uuid.Nil

	_, reason, err := h.svc.Refund(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrNoActor)
	assert.Equal(t, payment.ReasonNoActor, reason)
	assert.Equal(t, 0, h.prov.CallCount("Refund"))
}

func TestRefund_BadKey_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	reads := h.store.CallCount("IntentByID")

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "  "))

	require.ErrorIs(t, err, payment.ErrIdempotencyKeyInvalid)
	assert.Equal(t, payment.ReasonKeyInvalid, reason)
	assert.Equal(t, reads, h.store.CallCount("IntentByID"), "негодный ключ не стоит похода в БД")
}

func TestRefund_BeforeSettle_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.start(t, startReq())

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrNotSettled)
	assert.Equal(t, payment.ReasonNotSettled, reason)
	assert.Equal(t, 0, h.prov.CallCount("Refund"))
}

// Намерение оплачено, а денежной записи нет — это расхождение книг, а не
// «возврат нуля»: придумывать сумму из статуса значит вернуть деньги, которых
// не получали.
func TestRefund_SucceededWithoutCapture_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.store.Entries = nil

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrNotSettled)
	assert.Equal(t, payment.ReasonNotSettled, reason)
}

func TestRefund_UnknownIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, reason, err := h.svc.Refund(context.Background(), payment.RefundRequest{
		IntentID: uuid.New(), AmountMinor: 100, IdempotencyKey: "ref-1", ActorID: uuid.New(),
	})

	require.ErrorIs(t, err, payment.ErrUnknownIntent)
	assert.Equal(t, payment.ReasonUnknownIntent, reason)
}

func TestRefund_NoReceipt_RefusedBeforeProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	req := refundReq(in, 50000, "ref-1")
	req.Receipt = nil

	_, reason, err := h.svc.Refund(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrReceiptRequired)
	assert.Equal(t, payment.ReasonReceiptInvalid, reason)
	assert.Equal(t, 0, h.prov.CallCount("Refund"))
}

func TestRefund_ReceiptSumMismatch_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	req := refundReq(in, 50000, "ref-1")
	req.Receipt = receiptFor(50001)

	_, reason, err := h.svc.Refund(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrReceiptInvalid)
	assert.Equal(t, payment.ReasonReceiptInvalid, reason)
}

// Провайдер деньги вернул, а книга их не приняла: между нашей проверкой и
// записью проехал чужой возврат. Молчать нельзя — 503 с алертом, потому что
// деньги ушли, а строки нет.
func TestRefund_LedgerRefusesAfterProvider_IsLoud(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)
	h.store.RefundTooLargeOnce = true

	_, reason, err := h.svc.Refund(context.Background(), refundReq(in, 50000, "ref-1"))

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonRefundTooLarge, reason)
	assert.Equal(t, 1, h.prov.CallCount("Refund"), "деньги у провайдера уже ушли")
	assert.Empty(t, h.store.EntriesOf(in.ID, payment.LedgerRefund))
}

// Два оператора возвращают одновременно разными ключами: в книге не больше
// того, что зачислено, а проигравший получает ГРОМКИЙ отказ, а не тишину.
func TestConcurrent_Refund_DifferentKeys_LedgerHolds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.sold(t)

	var wg sync.WaitGroup
	reasons := make([]payment.Reason, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "ref-" + string(rune('a'+i))
			_, reasons[i], errs[i] = h.svc.Refund(context.Background(), refundReq(in, 70000, key))
		}()
	}
	wg.Wait()

	entries := h.store.EntriesOf(in.ID, payment.LedgerRefund)
	require.LessOrEqual(t, len(entries), 1, "два возврата по 70000 в зачисление 119800 не влезают")
	require.Len(t, entries, 1)
	var refused int
	for i := range 2 {
		if errs[i] != nil {
			refused++
			assert.Equal(t, payment.ReasonRefundTooLarge, reasons[i])
		}
	}
	assert.Equal(t, 1, refused, "ровно один отказ, и он назван")
}
