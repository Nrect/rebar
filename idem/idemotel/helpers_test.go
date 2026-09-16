package idemotel_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/idem"
)

// Имя и единица — контракт для алертов потребителя: тест держит их литералами.
const (
	requestsName = "idem_requests"
	unitRequest  = "{request}"
)

const (
	opCreate = idem.Operation("orders.create")
	opUpdate = idem.Operation("orders.update")
)

// operations — операции Config тестов: пар меток столько, сколько их ×
// idem.AllOutcomes.
var operations = []idem.Operation{opCreate, opUpdate}

func testConfig() idem.Config {
	return idem.Config{Operations: slices.Clone(operations), Retention: idem.MinRetention, MaxResponseBytes: 256}
}

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })
	return reader, mp.Meter("rebar.idem")
}

// requestPoints — точки счётчика одного scrape; имя, единица и монотонность —
// контракт.
func requestPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if m.Name != requestsName {
			continue
		}
		require.Equal(t, unitRequest, m.Unit)
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s обязан быть Int64Counter", requestsName)
		require.True(t, sum.IsMonotonic, "%s обязан быть счётчиком, а не UpDown", requestsName)
		return sum.DataPoints
	}
	require.Fail(t, "инструмента нет в scrape", requestsName)
	return nil
}

// requestCount — значение ряда операции и исхода; found = false — ряда нет.
func requestCount(points []metricdata.DataPoint[int64], op idem.Operation, outcome idem.Outcome) (value int64, found bool) {
	want := attribute.NewSet(attribute.String("operation", string(op)), attribute.String("outcome", string(outcome)))
	for _, dp := range points {
		if dp.Attributes.Equals(&want) {
			return dp.Value, true
		}
	}
	return 0, false
}

// assertClosedLabels — у каждой точки ровно операция и исход, оба из закрытых
// наборов: операция — из Config тестов, исход — из idem.AllOutcomes.
func assertClosedLabels(t *testing.T, points []metricdata.DataPoint[int64]) {
	t.Helper()
	for _, dp := range points {
		require.Equal(t, 2, dp.Attributes.Len(), "меток ровно две: operation и outcome")
		op, ok := dp.Attributes.Value("operation")
		require.True(t, ok, "у точки нет метки operation")
		outcome, ok := dp.Attributes.Value("outcome")
		require.True(t, ok, "у точки нет метки outcome")
		assert.Contains(t, operations, idem.Operation(op.AsString()), "метка operation вне Config.Operations")
		assert.Contains(t, idem.AllOutcomes, idem.Outcome(outcome.AsString()), "метка outcome вне idem.AllOutcomes")
	}
}

var errMeter = errors.New("meter: instrument refused")

// failingMeter — метр, отказывающий в счётчике: ошибка сборки возвращается.
type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errMeter
}
