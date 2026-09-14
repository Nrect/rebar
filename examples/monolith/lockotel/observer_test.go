package lockotel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/nrect/rebar/examples/monolith/lockotel"
)

// Ошибка сборки падает на старте: наблюдатель без метра или без задач не
// заведёт ни одного ряда, и алерт «ключ застрял» молчал бы всегда.
func TestNewObserver_PanicsOnMissingWiring(t *testing.T) {
	assert.PanicsWithValue(t, "lockotel.NewObserver: nil meter", func() {
		_, _ = lockotel.NewObserver(nil, "payments_reconcile")
	})
	assert.PanicsWithValue(t, "lockotel.NewObserver: no jobs", func() {
		_, _ = lockotel.NewObserver(noop.NewMeterProvider().Meter("rebar.pglock"))
	})
}
