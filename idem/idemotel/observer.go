package idemotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/idem"
)

// Имя и единица счётчика — часть контракта; Prometheus-экспортёр отрисует
// idem_requests_total.
const (
	requestsName  = "idem_requests"
	unitRequest   = "{request}"
	attrOperation = "operation"
	attrOutcome   = "outcome"
)

// observer — idem.Observer на metric API: idem_requests{operation,outcome}.
type observer struct{ requests metric.Int64Counter }

var _ idem.Observer = (*observer)(nil)

// NewObserver паникует на nil-метре и возвращает ошибку создания инструмента.
// Ряды заводит хранилище: его конструктор зовёт Watch по операциям Config.
func NewObserver(meter metric.Meter) (idem.Observer, error) {
	if meter == nil {
		panic("idemotel.NewObserver: nil meter")
	}
	requests, err := meter.Int64Counter(requestsName,
		metric.WithUnit(unitRequest),
		metric.WithDescription("Исходы Do хранилища idem по операции и исходу."),
	)
	if err != nil {
		return nil, fmt.Errorf("idemotel: инструмент %s: %w", requestsName, err)
	}
	return &observer{requests: requests}, nil
}

// Watch заводит ряды операции нулём по всем исходам (doc.go, п. 2).
func (o *observer) Watch(op idem.Operation) {
	for _, outcome := range idem.AllOutcomes {
		o.add(context.Background(), op, outcome, 0)
	}
}

// Outcome считает исход Do своей паре операции и исхода.
func (o *observer) Outcome(ctx context.Context, op idem.Operation, outcome idem.Outcome) {
	o.add(ctx, op, outcome, 1)
}

func (o *observer) add(ctx context.Context, op idem.Operation, outcome idem.Outcome, n int64) {
	o.requests.Add(ctx, n, metric.WithAttributes(
		attribute.String(attrOperation, string(op)),
		attribute.String(attrOutcome, string(outcome)),
	))
}
