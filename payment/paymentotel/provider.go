package paymentotel

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/payment"
)

// CallType — какой метод порта звали, метка type. Закрытый набор, а не имя
// метода из рефлексии: на этих строках стоят алерты потребителя.
type CallType string

const (
	CallCreatePayment CallType = "create_payment"
	CallParseWebhook  CallType = "parse_webhook"
	CallGetPayment    CallType = "get_payment"
	CallCapture       CallType = "capture"
	CallCancel        CallType = "cancel"
	CallRefund        CallType = "refund"
)

// AllCallTypes — полный список; держит guard-тест. Name здесь нет: см. doc.go, п. 4.
var AllCallTypes = []CallType{
	CallCreatePayment, CallParseWebhook, CallGetPayment, CallCapture, CallCancel, CallRefund,
}

// Result — исход вызова в метке result (закрытый набор: на нём алерты
// потребителя).
type Result string

const (
	ResultOK Result = "ok"
	// ResultRejected — провайдер ответил определённо и отрицательно, повтор
	// бессмысленен.
	ResultRejected Result = "rejected"
	// ResultError — ответа нет, повтор осмыслен.
	ResultError Result = "error"
)

// AllResults — полный список; держит guard-тест.
var AllResults = []Result{ResultOK, ResultRejected, ResultError}

// Имя и единица счётчика — часть контракта; Prometheus-экспортёр отрисует
// payment_provider_calls_total.
const (
	callsName = "payment_provider_calls"
	unitCall  = "{call}"
)

// Метки счётчика: оба набора закрыты, см. doc.go, п. 1.
const (
	attrType   = "type"
	attrResult = "result"
)

// rejectedClasses — классы ядра, которыми адаптер говорит «ответ есть, и он
// отрицательный». Всё, чего здесь нет, считается молчанием.
var rejectedClasses = []error{
	payment.ErrProviderRejected, // отказ по существу
	payment.ErrUnsupported,      // операции у адаптера нет по конструкции
	payment.ErrInvalidSignature, // вебхук не подтверждён: 400, повтора не будет
	payment.ErrMalformedEvent,   // тело не разбирается: 400, повтора не будет
}

// provider — декоратор payment.Provider: каждый вызов считается в
// payment_provider_calls{type,result}. Потокобезопасен, если потокобезопасен next.
type provider struct {
	next  payment.Provider
	calls metric.Int64Counter
}

var _ payment.Provider = (*provider)(nil)

// Wrap паникует на nil-порте и nil-метре, как payment.NewService: ошибка сборки
// обязана падать на старте. Ошибка создания инструмента возвращается — метрик
// у потребителя может не быть, а платежи нужны.
func Wrap(next payment.Provider, meter metric.Meter) (payment.Provider, error) {
	if next == nil {
		panic("paymentotel.Wrap: nil provider")
	}
	if meter == nil {
		panic("paymentotel.Wrap: nil meter")
	}
	calls, err := meter.Int64Counter(callsName,
		metric.WithUnit(unitCall),
		metric.WithDescription("Вызовы платёжного провайдера по методу и исходу."),
	)
	if err != nil {
		return nil, fmt.Errorf("paymentotel: инструмент %s: %w", callsName, err)
	}
	return &provider{next: next, calls: calls}, nil
}

// Name отдаёт имя next как есть и не считается: см. doc.go, п. 4.
func (p *provider) Name() payment.ProviderName { return p.next.Name() }

// CreatePayment — отказ создания порт сообщает не ошибкой, а Status ==
// EventFailed (CreatePaymentResult.Status), и это тоже rejected.
func (p *provider) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest,
) (payment.CreatePaymentResult, error) {
	res, err := p.next.CreatePayment(ctx, req)
	result := classify(err)
	if err == nil && res.Status == payment.EventFailed {
		result = ResultRejected
	}
	p.count(ctx, CallCreatePayment, result)
	return res, err
}

func (p *provider) ParseWebhook(ctx context.Context, req payment.WebhookRequest) (payment.Event, error) {
	ev, err := p.next.ParseWebhook(ctx, req)
	p.count(ctx, CallParseWebhook, classify(err))
	return ev, err
}

func (p *provider) GetPayment(ctx context.Context, providerPaymentID string) (payment.Event, error) {
	ev, err := p.next.GetPayment(ctx, providerPaymentID)
	p.count(ctx, CallGetPayment, classify(err))
	return ev, err
}

func (p *provider) Capture(ctx context.Context, req payment.CaptureRequest) (payment.Event, error) {
	ev, err := p.next.Capture(ctx, req)
	p.count(ctx, CallCapture, classify(err))
	return ev, err
}

func (p *provider) Cancel(ctx context.Context, providerPaymentID, idempotencyKey string) (payment.Event, error) {
	ev, err := p.next.Cancel(ctx, providerPaymentID, idempotencyKey)
	p.count(ctx, CallCancel, classify(err))
	return ev, err
}

func (p *provider) Refund(ctx context.Context, req payment.RefundProviderRequest) (payment.Event, error) {
	ev, err := p.next.Refund(ctx, req)
	p.count(ctx, CallRefund, classify(err))
	return ev, err
}

// count — одна точка записи на все методы; ответ и ошибку next не трогает
// (doc.go, п. 3), в метки берёт только метод и исход (п. 1).
func (p *provider) count(ctx context.Context, call CallType, result Result) {
	p.calls.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrType, string(call)),
		attribute.String(attrResult, string(result)),
	))
}

// classify — исход по КЛАССУ ошибки через errors.Is: декоратор не знает, чей
// адаптер под ним, и текста ошибки не читает.
//
// ОТКАЗ И МОЛЧАНИЕ НЕ СВОДЯТСЯ. «Провайдер отказал» — обычная работа, таких
// сотни в день; «провайдер не ответил» — инцидент. Сведи их в один исход — и
// алерт «провайдер недоступен» будет гореть от фонового шума отказов, а через
// неделю его отключат. Поэтому неизвестная ошибка — error: отказ обязан
// назвать адаптер классом ядра, а молчание по умолчанию тревожит.
//
// Класс отказа ищется раньше молчания, как в providerError ядра:
// ErrUnavailable поверх ErrUnsupported — всё ещё отказ по конструкции.
func classify(err error) Result {
	if err == nil {
		return ResultOK
	}
	for _, class := range rejectedClasses {
		if errors.Is(err, class) {
			return ResultRejected
		}
	}
	return ResultError
}
