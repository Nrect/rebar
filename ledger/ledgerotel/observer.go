package ledgerotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/ledger"
)

// Имя и единица счётчика — часть контракта; Prometheus-экспортёр отрисует
// ledger_reconcile_mismatch_total.
const (
	mismatchName = "ledger_reconcile_mismatch"
	unitMismatch = "{mismatch}"
	attrBook     = "book"
	attrCheck    = "check"
)

// observer — ledger.Observer на metric API.
type observer struct{ mismatches metric.Int64Counter }

var _ ledger.Observer = (*observer)(nil)

// NewObserver паникует на nil-метре и возвращает ошибку создания инструмента.
func NewObserver(meter metric.Meter) (ledger.Observer, error) {
	if meter == nil {
		panic("ledgerotel.NewObserver: nil meter")
	}
	mismatches, err := meter.Int64Counter(mismatchName,
		metric.WithUnit(unitMismatch),
		metric.WithDescription("Расхождения сверки журнала по книге и проверке."),
	)
	if err != nil {
		return nil, fmt.Errorf("ledgerotel: инструмент %s: %w", mismatchName, err)
	}
	return &observer{mismatches: mismatches}, nil
}

// Watch заводит ряды книги нулём по всем проверкам (doc.go, п. 2).
func (o *observer) Watch(book string) {
	for _, check := range ledger.AllChecks {
		o.add(context.Background(), book, check, 0)
	}
}

// Found считает каждое расхождение своей проверкой.
func (o *observer) Found(ctx context.Context, f ledger.Finding) {
	for _, m := range f.Mismatches {
		o.add(ctx, f.Book, m.Check, 1)
	}
}

func (o *observer) add(ctx context.Context, book string, check ledger.Check, n int64) {
	o.mismatches.Add(ctx, n, metric.WithAttributes(
		attribute.String(attrBook, book),
		attribute.String(attrCheck, string(check)),
	))
}
