package inboxotel

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/inbox"
)

// Имена, единицы и границы корзин — часть контракта; Prometheus-экспортёр
// отрисует inbox_received_total и inbox_receive_duration_seconds.
const (
	receivedName = "inbox_received"
	durationName = "inbox_receive_duration"
	unitDelivery = "{delivery}"
	unitSeconds  = "s"
	attrSource   = "source"
	attrOutcome  = "outcome"
)

// durationBounds — границы корзин в секундах; почему такие — doc.go.
var durationBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 15, 30}

// observer — inbox.Observer на metric API.
type observer struct {
	received metric.Int64Counter
	duration metric.Float64Histogram
}

var _ inbox.Observer = (*observer)(nil)

// NewObserver паникует на nil-метре и возвращает ошибку создания инструмента.
func NewObserver(meter metric.Meter) (inbox.Observer, error) {
	if meter == nil {
		panic("inboxotel.NewObserver: nil meter")
	}
	received, err := meter.Int64Counter(receivedName,
		metric.WithUnit(unitDelivery),
		metric.WithDescription("Доставки вебхуков по источнику и исходу."),
	)
	if err != nil {
		return nil, fmt.Errorf("inboxotel: инструмент %s: %w", receivedName, err)
	}
	duration, err := meter.Float64Histogram(durationName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Время приёма доставки: проверка, транзакция, обработчик."),
		metric.WithExplicitBucketBoundaries(durationBounds...),
	)
	if err != nil {
		return nil, fmt.Errorf("inboxotel: инструмент %s: %w", durationName, err)
	}
	return &observer{received: received, duration: duration}, nil
}

// Watch заводит ряды источника нулём по всем исходам (doc.go, п. 2);
// гистограмму — нет (п. 3). Повторный Watch рядов не удваивает: у двух
// сервисов бывает один наблюдатель.
func (o *observer) Watch(source inbox.SourceName) {
	for _, outcome := range inbox.AllOutcomes {
		o.received.Add(context.Background(), 0, series(source, outcome))
	}
}

// Received считает доставку и её время.
func (o *observer) Received(ctx context.Context, source inbox.SourceName, outcome inbox.Outcome, took time.Duration) {
	o.received.Add(ctx, 1, series(source, outcome))
	o.duration.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String(attrSource, string(source))))
}

// series — метки ряда счётчика: оба набора закрыты (doc.go, п. 1).
func series(source inbox.SourceName, outcome inbox.Outcome) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String(attrSource, string(source)),
		attribute.String(attrOutcome, string(outcome)),
	)
}
