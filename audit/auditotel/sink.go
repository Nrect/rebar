package auditotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/audit"
)

// Имя и единица счётчика — часть контракта; Prometheus-экспортёр отрисует
// audit_events_total.
const (
	counterName = "audit_events"
	unitEvent   = "{event}"
)

// Метки счётчика: оба набора закрыты, см. doc.go, п. 1.
const (
	attrAction  = "action"
	attrOutcome = "outcome"
)

// Sink — декоратор audit.Sink: каждое записанное событие считается в
// audit_events{action,outcome}. Потокобезопасен, если потокобезопасен next.
type Sink struct {
	next   audit.Sink
	events metric.Int64Counter
}

var _ audit.Sink = (*Sink)(nil)

// Wrap паникует на nil-порте и nil-метре, как audit.NewRecorder: ошибка сборки
// обязана падать на старте. Ошибка создания инструмента возвращается — метрик
// у потребителя может не быть, а журнал нужен.
func Wrap(next audit.Sink, meter metric.Meter) (*Sink, error) {
	if next == nil {
		panic("auditotel.Wrap: nil sink")
	}
	if meter == nil {
		panic("auditotel.Wrap: nil meter")
	}
	events, err := meter.Int64Counter(counterName,
		metric.WithUnit(unitEvent),
		metric.WithDescription("Записанные события журнала действий."),
	)
	if err != nil {
		return nil, fmt.Errorf("auditotel: инструмент %s: %w", counterName, err)
	}
	return &Sink{next: next, events: events}, nil
}

// Write считает событие ПОСЛЕ успешной записи и отдаёт ошибку next без
// изменений: цепочка вызывающего (errors.Is) обязана пережить декоратор.
//
// СЧИТАЕТСЯ ЗАПИСАННОЕ, А НЕ ПОПЫТКА. Непрошедшая запись — это либо откат
// транзакции, в которой не случилось и само действие (WithTx), либо сбой,
// о котором вызывающему сказано ошибкой. Счёт попыток нарисовал бы на
// дашборде события, которых в журнале нет, и разбор инцидента пошёл бы по
// метрике, а не по журналу.
func (s *Sink) Write(ctx context.Context, ev audit.Event) error {
	if err := s.next.Write(ctx, ev); err != nil {
		return err
	}
	s.events.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrAction, string(ev.Action)),
		attribute.String(attrOutcome, string(ev.Outcome)),
	))
	return nil
}
