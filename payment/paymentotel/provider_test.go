package paymentotel_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Исход считается по КЛАССУ ошибки ядра на каждом методе порта: декоратор не
// знает, чей адаптер под ним, и обёртка адаптера класс не прячет.
func TestWrap_ClassifiesByErrorClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want paymentotel.Result
	}{
		{"успех", nil, paymentotel.ResultOK},
		{"отказ по существу", payment.ErrProviderRejected, paymentotel.ResultRejected},
		{"отказ под обёрткой адаптера", fmt.Errorf("adapter: 422: %w", payment.ErrProviderRejected), paymentotel.ResultRejected},
		{"операции у адаптера нет", fmt.Errorf("adapter: holds: %w", payment.ErrUnsupported), paymentotel.ResultRejected},
		{"вебхук не подтверждён", payment.ErrInvalidSignature, paymentotel.ResultRejected},
		{"тело вебхука не разбирается", fmt.Errorf("%w: bad json", payment.ErrMalformedEvent), paymentotel.ResultRejected},
		{"провайдер не ответил", fmt.Errorf("%w: 503", payment.ErrUnavailable), paymentotel.ResultError},
		{"таймаут", context.DeadlineExceeded, paymentotel.ResultError},
		{"обрыв соединения", io.ErrUnexpectedEOF, paymentotel.ResultError},
		{"неизвестная ошибка — молчание, а не отказ", errors.New("adapter: odd"), paymentotel.ResultError},
	}
	for _, pc := range everyCall() {
		for _, tc := range cases {
			t.Run(string(pc.typ)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				p, reader := wrap(t, newStub(tc.err))

				err := pc.do(context.Background(), p)

				if tc.err == nil {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, tc.err)
				}
				ms := collect(t, reader)
				assert.Len(t, callPoints(t, ms), 1, "ровно одна пара меток")
				assert.Equal(t, int64(1), callCount(t, ms, pc.typ, tc.want))
			})
		}
	}
}

// Отказ создания платежа порт сообщает не ошибкой, а Status == EventFailed:
// без этой ветки create_payment/rejected был бы вечным нулём, а отказы
// считались бы успехами.
func TestWrap_CreatePaymentFailedStatusIsRejected(t *testing.T) {
	t.Parallel()
	cases := map[payment.EventType]paymentotel.Result{
		payment.EventFailed:  paymentotel.ResultRejected,
		payment.EventPending: paymentotel.ResultOK,
	}
	for status, want := range cases {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			stub := newStub(nil)
			stub.res = payment.CreatePaymentResult{Status: status}
			p, reader := wrap(t, stub)

			res, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{})

			require.NoError(t, err)
			assert.Equal(t, status, res.Status, "ответ next как есть")
			assert.Equal(t, int64(1), callCount(t, collect(t, reader), paymentotel.CallCreatePayment, want))
		})
	}
}

// Состояние платежа в ответе прочих методов — ответ, а не отказ: сверка,
// узнавшая «платёж не прошёл», провайдера дозвалась.
func TestWrap_FailedPaymentStateIsAnAnswer(t *testing.T) {
	t.Parallel()
	stub := newStub(nil)
	stub.ev = payment.Event{Type: payment.EventFailed}
	p, reader := wrap(t, stub)

	_, err := p.GetPayment(context.Background(), "pay-1")

	require.NoError(t, err)
	assert.Equal(t, int64(1), callCount(t, collect(t, reader), paymentotel.CallGetPayment, paymentotel.ResultOK))
}

// Класс отказа ищется раньше молчания: ErrUnavailable поверх ErrUnsupported —
// всё ещё отказ по конструкции, как в providerError ядра.
func TestWrap_RejectedClassWinsOverUnavailable(t *testing.T) {
	t.Parallel()
	p, reader := wrap(t, newStub(fmt.Errorf("%w: capture: %w", payment.ErrUnavailable, payment.ErrUnsupported)))

	_, err := p.Capture(context.Background(), payment.CaptureRequest{})

	require.ErrorIs(t, err, payment.ErrUnsupported)
	assert.Equal(t, int64(1), callCount(t, collect(t, reader), paymentotel.CallCapture, paymentotel.ResultRejected))
}

// adapterError — типизированная ошибка адаптера: errors.As вызывающего обязан
// пережить декоратор так же, как errors.Is.
type adapterError struct{ code string }

func (e *adapterError) Error() string { return "adapter: " + e.code }
func (e *adapterError) Unwrap() error { return payment.ErrProviderRejected }

// До next доходят ТЕ ЖЕ контекст и аргументы, а вызывающему — ТОТ ЖЕ ответ и
// та же ошибка: сервис ветвится по errors.Is и errors.As.
func TestWrap_PassesEverythingThrough(t *testing.T) {
	t.Parallel()
	declined := &adapterError{code: "card_declined"}
	stub := newStub(declined)
	stub.res = payment.CreatePaymentResult{ProviderPaymentID: "pay-1", Status: payment.EventPending}
	stub.ev = payment.Event{ProviderEventID: "stub:pay-1:succeeded", Type: payment.EventSucceeded}
	p, _ := wrap(t, stub)
	const marker = "контекст вызывающего"
	ctx := context.WithValue(context.Background(), markerKey{}, marker)

	create := payment.CreatePaymentRequest{IntentID: uuid.New(), Reference: "order-42", AmountMinor: 119800, Currency: "RUB"}
	webhook := payment.WebhookRequest{
		Raw: []byte(`{"id":"pay-1"}`), Headers: map[string][]string{"X-Signature": {"abc"}}, RemoteIP: "203.0.113.7",
	}
	capture := payment.CaptureRequest{ProviderPaymentID: "pay-1", AmountMinor: 119800, Currency: "RUB", IdempotencyKey: "shop:c:1"}
	refund := payment.RefundProviderRequest{ProviderPaymentID: "pay-1", AmountMinor: 500, Currency: "RUB", IdempotencyKey: "shop:r:1"}

	res, err := p.CreatePayment(ctx, create)
	assert.Equal(t, stub.res, res)
	requireSameError(t, err, declined)
	for _, do := range []func() (payment.Event, error){
		func() (payment.Event, error) { return p.ParseWebhook(ctx, webhook) },
		func() (payment.Event, error) { return p.GetPayment(ctx, "pay-1") },
		func() (payment.Event, error) { return p.Capture(ctx, capture) },
		func() (payment.Event, error) { return p.Cancel(ctx, "pay-1", "shop:x:1") },
		func() (payment.Event, error) { return p.Refund(ctx, refund) },
	} {
		ev, evErr := do()
		assert.Equal(t, stub.ev, ev)
		requireSameError(t, evErr, declined)
	}

	assert.Equal(t, []seenCall{
		{"CreatePayment", marker, []any{create}},
		{"ParseWebhook", marker, []any{webhook}},
		{"GetPayment", marker, []any{"pay-1"}},
		{"Capture", marker, []any{capture}},
		{"Cancel", marker, []any{"pay-1", "shop:x:1"}},
		{"Refund", marker, []any{refund}},
	}, stub.calls)
}

// requireSameError — ошибка next дошла как есть: и класс ядра, и тип адаптера.
func requireSameError(t *testing.T, got error, want *adapterError) {
	t.Helper()
	require.ErrorIs(t, got, payment.ErrProviderRejected)
	var typed *adapterError
	require.ErrorAs(t, got, &typed)
	assert.Same(t, want, typed)
}

// Name пробрасывается как есть — по нему сервис узнаёт провайдера в каждом
// событии — и не считается: вызова провайдера за ним нет.
func TestWrap_NameIsPassedThroughAndNotCounted(t *testing.T) {
	t.Parallel()
	stub := newStub(nil)
	stub.name = "yookassa"
	p, reader := wrap(t, stub)

	assert.Equal(t, payment.ProviderName("yookassa"), p.Name())
	assert.Equal(t, 1, stub.names)
	assert.Empty(t, callPoints(t, collect(t, reader)), "Name счётчик не трогает")
}

// Декоратор подставляется вместо порта: сервис собирается на нём, имя проходит
// проверку формы, а классы ошибок доезжают до Reason ядра.
func TestWrap_DropsInForThePort(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	prov := paymenttest.NewMemProvider("memprov")
	prov.RejectFor["order-rejected"] = true
	p, err := paymentotel.Wrap(prov, meter)
	require.NoError(t, err)
	svc := payment.NewService(paymenttest.NewMemStore(), p, serviceConfig())
	svc.SetClock(paymenttest.NewClock(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)).Now)
	ctx := context.Background()

	_, _, err = svc.Start(ctx, startRequest("order-ok"))
	require.NoError(t, err)
	_, reason, err := svc.Start(ctx, startRequest("order-rejected"))
	require.ErrorIs(t, err, payment.ErrProviderRejected)
	assert.Equal(t, payment.ReasonProviderRejected, reason)

	prov.BadSignature = true
	_, reason, err = svc.HandleWebhook(ctx, payment.WebhookRequest{Raw: []byte(`{}`)})
	require.ErrorIs(t, err, payment.ErrInvalidSignature)
	assert.Equal(t, payment.ReasonSignatureInvalid, reason)

	// Проверочное чтение не удалось: ErrUnavailable обязан доехать до сервиса,
	// иначе тот ответит провайдеру 400 вместо 503.
	prov.BadSignature = false
	prov.ParseErr = fmt.Errorf("%w: verification read", payment.ErrUnavailable)
	_, reason, err = svc.HandleWebhook(ctx, payment.WebhookRequest{Raw: []byte(`{}`)})
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderError, reason, "503, а не malformed_event")

	assert.Equal(t, payment.ProviderName("memprov"), svc.Provider())
	ms := collect(t, reader)
	assert.Equal(t, int64(1), callCount(t, ms, paymentotel.CallCreatePayment, paymentotel.ResultOK))
	assert.Equal(t, int64(1), callCount(t, ms, paymentotel.CallCreatePayment, paymentotel.ResultRejected))
	assert.Equal(t, int64(1), callCount(t, ms, paymentotel.CallParseWebhook, paymentotel.ResultRejected))
	assert.Equal(t, int64(1), callCount(t, ms, paymentotel.CallParseWebhook, paymentotel.ResultError))
}

// Таймаут — главный источник error, и ctx к моменту записи уже отменён: вызов
// обязан попасть в счётчик всё равно, иначе алерт «провайдер недоступен» слеп
// ровно там, где нужен.
func TestWrap_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	p, reader := wrap(t, newStub(context.DeadlineExceeded))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.GetPayment(ctx, "pay-1")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int64(1), callCount(t, collect(t, reader), paymentotel.CallGetPayment, paymentotel.ResultError))
}

// Ни сумма, ни валюта, ни id намерения, ни ссылка заказа, ни текст ошибки в
// метки не попадают: это данные, а не словарь.
func TestWrap_NoDataInLabels(t *testing.T) {
	t.Parallel()
	intentID := uuid.New()
	leaky := fmt.Errorf("%w: card 4111111111111111 declined for order-777", payment.ErrProviderRejected)
	p, reader := wrap(t, newStub(leaky))

	_, err := p.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		IntentID: intentID, PayerID: uuid.New(), Reference: "order-777",
		AmountMinor: 119800, Currency: "RUB", Description: "Подписка для buyer@example.com",
	})
	require.Error(t, err)

	points := callPoints(t, collect(t, reader))
	require.Len(t, points, 1)
	for _, kv := range points[0].Attributes.ToSlice() {
		assert.Contains(t, []string{"type", "result"}, string(kv.Key), "лишняя метка")
		for _, data := range []string{"4111", "order-777", "119800", "RUB", intentID.String(), "buyer@"} {
			assert.NotContains(t, kv.Value.AsString(), data)
		}
	}
}

// Nil-порт и nil-метр — паника на старте, как у payment.NewService.
func TestWrap_PanicsOnNil(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)

	assert.Panics(t, func() { _, _ = paymentotel.Wrap(nil, meter) })
	assert.Panics(t, func() { _, _ = paymentotel.Wrap(newStub(nil), nil) })
}

// Отказ метра в инструменте возвращается, а не роняет процесс: метрик у
// потребителя может не быть, а платежи нужны.
func TestWrap_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	var (
		p   payment.Provider
		err error
	)

	assert.NotPanics(t, func() { p, err = paymentotel.Wrap(newStub(nil), failingMeter{failOn: callsName}) })

	require.ErrorIs(t, err, errMeter)
	assert.Nil(t, p)
}

func serviceConfig() payment.Config {
	return payment.Config{
		Currency:          "RUB",
		MaxAmountMinor:    10_000_000,
		MaxItems:          10,
		IntentTTL:         30 * time.Minute,
		StalePendingAfter: 15 * time.Minute,
		ProviderKeyPrefix: "shop",
	}
}

func startRequest(reference string) payment.StartRequest {
	return payment.StartRequest{
		PayerID:   uuid.MustParse("7d3f7c5e-4f0a-4c1e-9a55-0b8f3c2a9e11"),
		Reference: reference,
		Items: []payment.OrderItem{
			{Position: 0, ProductID: "book-1", Title: "Книга", AmountMinor: 79900, Quantity: 1},
		},
		AmountMinor:    79900,
		Currency:       "RUB",
		IdempotencyKey: "buy-" + reference,
	}
}
