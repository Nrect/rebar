package paymentotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/payment"
)

// Имя и единица счётчика исходов — часть контракта; Prometheus-экспортёр
// отрисует payments_total.
const (
	outcomesName  = "payments"
	unitOperation = "{operation}"
	attrOp        = "op"
	attrReason    = "reason"
)

// observer — payment.Observer на metric API: payments{op,reason}.
type observer struct{ outcomes metric.Int64Counter }

var _ payment.Observer = (*observer)(nil)

// NewObserver паникует на nil-метре и возвращает ошибку создания инструмента,
// как Wrap.
//
// КАЖДЫЙ РЯД РОЖДАЕТСЯ НУЛЁМ. Ряд, появившийся сразу единицей, increase() не
// видит: первого инкремента для него нет. А у status_conflict и amount_mismatch
// порог алерта 1 — первое же событие и есть тревога. Поэтому все пары
// AllOps × AllReasons заводятся при сборке.
func NewObserver(meter metric.Meter) (payment.Observer, error) {
	if meter == nil {
		panic("paymentotel.NewObserver: nil meter")
	}
	outcomes, err := meter.Int64Counter(outcomesName,
		metric.WithUnit(unitOperation),
		metric.WithDescription("Исходы операций платежей по операции и причине."),
	)
	if err != nil {
		return nil, fmt.Errorf("paymentotel: инструмент %s: %w", outcomesName, err)
	}
	o := &observer{outcomes: outcomes}
	for _, op := range payment.AllOps {
		for _, reason := range payment.AllReasons {
			o.add(context.Background(), op, reason, 0)
		}
	}
	return o, nil
}

// Outcome считает исход; op и reason — закрытые наборы ядра (doc.go, п. 1).
func (o *observer) Outcome(ctx context.Context, op payment.Op, reason payment.Reason) {
	o.add(ctx, op, reason, 1)
}

func (o *observer) add(ctx context.Context, op payment.Op, reason payment.Reason, n int64) {
	o.outcomes.Add(ctx, n, metric.WithAttributes(
		attribute.String(attrOp, string(op)),
		attribute.String(attrReason, string(reason)),
	))
}
