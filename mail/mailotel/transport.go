package mailotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/mail"
)

// Result — исход попытки отправки в метке result (закрытый набор: на нём
// алерты потребителя).
type Result string

const (
	ResultOK Result = "ok"
	// ResultRejected — провайдер ответил определённо, повтор бессмысленен.
	ResultRejected Result = "rejected"
	// ResultError — ответа нет, повтор осмыслен.
	ResultError Result = "error"
)

// AllResults — полный список; держит guard-тест.
var AllResults = []Result{ResultOK, ResultRejected, ResultError}

// Имя и единица счётчика — часть контракта; Prometheus-экспортёр отрисует
// emails_sent_total.
const (
	counterName = "emails_sent"
	unitEmail   = "{email}"
)

// Метки счётчика: оба набора закрыты, см. doc.go, п. 1.
const (
	attrType   = "type"
	attrResult = "result"
)

// Transport — декоратор mail.Transport: каждый Send считается в
// emails_sent{type,result}. Потокобезопасен, если потокобезопасен next.
type Transport struct {
	next mail.Transport
	sent metric.Int64Counter
}

var _ mail.Transport = (*Transport)(nil)

// Wrap паникует на nil-порте и nil-метре, как mail.NewService: ошибка сборки
// обязана падать на старте. Ошибка создания инструмента возвращается —
// метрик у потребителя может не быть, а почта нужна.
func Wrap(next mail.Transport, meter metric.Meter) (*Transport, error) {
	switch {
	case next == nil:
		panic("mailotel.Wrap: nil transport")
	case meter == nil:
		panic("mailotel.Wrap: nil meter")
	}
	sent, err := meter.Int64Counter(counterName,
		metric.WithUnit(unitEmail),
		metric.WithDescription("Попытки отправки письма транспортом."),
	)
	if err != nil {
		return nil, fmt.Errorf("mailotel: инструмент %s: %w", counterName, err)
	}
	return &Transport{next: next, sent: sent}, nil
}

// Name отдаёт имя next как есть: см. doc.go, п. 3.
func (t *Transport) Name() mail.TransportName { return t.next.Name() }

// Send считает попытку и отдаёт результат и ошибку next без изменений: цепочка
// обёрток вызывающего (errors.Is, errors.As) обязана пережить декоратор.
func (t *Transport) Send(ctx context.Context, env mail.Envelope) (mail.SendResult, error) {
	res, err := t.next.Send(ctx, env)
	t.sent.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrType, string(env.Kind)),
		attribute.String(attrResult, string(classify(err))),
	))
	return res, err
}

// classify — три исхода, а не два: см. doc.go, п. 5.
func classify(err error) Result {
	switch {
	case err == nil:
		return ResultOK
	case mail.IsRejected(err):
		return ResultRejected
	default:
		return ResultError
	}
}
