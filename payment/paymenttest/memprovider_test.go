package paymenttest_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Публичных полей у двойников с замком нет: всё, что методы читают под
// мьютексом, правится только методами под ним же. Новое поле-ручка краснеет
// здесь, а не гонкой у потребителя.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[paymenttest.MemProvider](),
		reflect.TypeFor[paymenttest.MemStore](),
		reflect.TypeFor[paymenttest.Observer](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Ручки правятся на ходу: тест потребителя меняет их, пока ручка его
// HTTP-сервера в другой горутине зовёт провайдер. Под -race это обязано быть
// чисто — иначе краснело бы у потребителя, а не у нас.
func TestMemProvider_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	ctx := context.Background()
	stop := make(chan struct{})
	served := make(chan struct{})

	go func() { // ручка сервера
		defer close(served)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("k-%d", i)
			_, _ = prov.CreatePayment(ctx, payment.CreatePaymentRequest{
				IntentID: uuid.New(), Reference: "order:" + key, IdempotencyKey: key,
			})
			_, _ = prov.ParseWebhook(ctx, payment.WebhookRequest{})
			_, _ = prov.GetPayment(ctx, "pay-x")
			_, _ = prov.Capture(ctx, payment.CaptureRequest{ProviderPaymentID: "pay-x", IdempotencyKey: "c-" + key})
			_, _ = prov.Cancel(ctx, "pay-x", "x-"+key)
			_, _ = prov.Refund(ctx, payment.RefundProviderRequest{ProviderPaymentID: "pay-x", IdempotencyKey: "r-" + key})
		}
	}()

	for i := range 200 { // горутина теста
		var fail error
		if i%2 == 0 {
			fail = paymenttest.ErrProviderDown
		}
		prov.SetCreateErr(fail)
		prov.SetGetErr(fail)
		prov.SetCaptureErr(fail)
		prov.SetCancelErr(fail)
		prov.SetRefundErr(fail)
		prov.SetParseErr(fail)
		prov.SetNoHolds(i%3 == 0)
		prov.SetBadSignature(i%5 == 0)
		prov.SetNoPaymentID(i%7 == 0)
		prov.SetResult(payment.CreatePaymentResult{})
		prov.SetRefundEcho(int64(i))
		prov.RejectNext()
		prov.RejectReference(fmt.Sprintf("order:k-%d", i))
		prov.FailReference("order:never")
		prov.SetCreateHook(func(payment.CreatePaymentRequest) {})
		prov.Push(payment.Event{})
		prov.SetPayment("pay-x", payment.Event{Type: payment.EventAuthorized})
		_, _, _, _ = prov.Created(), prov.Captures(), prov.Cancels(), prov.Refunds()
		_ = prov.RefundKeys()
		_ = prov.CallCount("CreatePayment")
	}
	close(stop)
	<-served
}

// Хук зовётся вне замка: он изображает событие, приехавшее между вставкой
// намерения и ответом провайдера, и вправе трогать сам провайдер — под замком
// это была бы взаимная блокировка.
func TestMemProvider_CreateHookMayTouchTheProvider(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	prov.SetCreateHook(func(req payment.CreatePaymentRequest) {
		prov.Push(prov.Event("pay-early", req.IntentID, payment.EventSucceeded, 100, "RUB"))
	})

	done := make(chan error, 1)
	go func() {
		_, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
			IntentID: uuid.New(), IdempotencyKey: "k-1",
		})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("хук, тронувший провайдер, повесил CreatePayment: он зовётся под замком")
	}

	ev, err := prov.ParseWebhook(context.Background(), payment.WebhookRequest{})
	require.NoError(t, err)
	assert.Equal(t, "memprov:pay-early:succeeded", ev.ProviderEventID)
}

// RejectNext не требует знать ссылку заказа: HTTP-тест потребителя её заранее
// не знает. Отказ достаётся следующему НОВОМУ платежу, повтор по ключу отдаёт
// прежний ответ, а следующий платёж проходит.
func TestMemProvider_RejectNext(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	ctx := context.Background()
	req := func(key string) payment.CreatePaymentRequest {
		return payment.CreatePaymentRequest{IntentID: uuid.New(), Reference: "order:" + key, IdempotencyKey: key}
	}
	prov.RejectNext()

	first, err := prov.CreatePayment(ctx, req("k-1"))
	require.NoError(t, err)
	assert.Equal(t, payment.EventFailed, first.Status, "отказ детерминированный, а не сбой связи")

	replay, err := prov.CreatePayment(ctx, req("k-1"))
	require.NoError(t, err)
	assert.Equal(t, first, replay, "повтор по ключу — тот же отказ")

	second, err := prov.CreatePayment(ctx, req("k-2"))
	require.NoError(t, err)
	assert.Equal(t, payment.EventPending, second.Status, "отказ был один")
}

// Записанные запросы отдаются копией: правка полученного среза состояние
// двойника не меняет.
func TestMemProvider_RecordedRequestsAreCopies(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	_, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		IntentID: uuid.New(), Reference: "order:1", IdempotencyKey: "k-1",
	})
	require.NoError(t, err)

	got := prov.Created()
	got[0].Reference = "order:tampered"

	assert.Equal(t, "order:1", prov.Created()[0].Reference)
}
