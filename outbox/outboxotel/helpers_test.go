package outboxotel_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxotel"
)

// newHandlers — декоратор на ручном ридере метрик и записывающем экспортёре
// span'ов: тест читает то, что реально ушло бы в бэкенд.
func newHandlers(t *testing.T) (*outboxotel.Handlers, *metric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()
	reader := metric.NewManualReader()
	spans := tracetest.NewSpanRecorder()
	h, err := outboxotel.New(
		metric.NewMeterProvider(metric.WithReader(reader)).Meter("rebar.outbox"),
		sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("rebar.outbox"),
		workerConfig(),
	)
	require.NoError(t, err)
	return h, reader, spans
}

// workerConfig — тот же Config, что уходит воркеру: MaxAttempts у декоратора и
// у Drain обязан быть один.
func workerConfig() outbox.Config {
	return outbox.Config{
		Kinds:           []outbox.Kind{testKind},
		MaxAttempts:     maxAttempts,
		Backoff:         outbox.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           time.Minute,
		HandlerTimeout:  10 * time.Second,
		BatchSize:       10,
		Retention:       time.Hour,
		MaxPayloadBytes: 4096,
	}
}

// handle — одна доставка через обёртку. Что Register кладёт в реестр именно
// её, проверяет отдельный тест через Worker.Drain.
func handle(t *testing.T, h *outboxotel.Handlers, next outbox.Handler, d outbox.Delivery) {
	t.Helper()
	_ = h.Wrap(d.Kind, next).Handle(t.Context(), d)
}

// counter — точки счётчика outbox_handled из ручного ридера.
func counter(t *testing.T, reader *metric.ManualReader) metricdata.Sum[int64] {
	t.Helper()
	sum, ok := instrument[metricdata.Sum[int64]](t, reader, "outbox_handled")
	if !ok {
		return metricdata.Sum[int64]{}
	}
	return sum
}

func histogram(t *testing.T, reader *metric.ManualReader) metricdata.Histogram[float64] {
	t.Helper()
	hist, ok := instrument[metricdata.Histogram[float64]](t, reader, "outbox_handle_duration")
	require.True(t, ok, "инструмента outbox_handle_duration нет")
	return hist
}

// counterValue — значение счётчика для пары меток; ноль, если такой пары нет.
func counterValue(t *testing.T, reader *metric.ManualReader, kind outbox.Kind, result outboxotel.Result) int64 {
	t.Helper()
	want := attribute.NewSet(
		attribute.String("kind", string(kind)),
		attribute.String("result", string(result)),
	)
	for _, dp := range counter(t, reader).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

// gaugeValue — значение observable gauge по имени; ok = false, если инструмент
// в scrape не попал.
func gaugeValue[N int64 | float64](t *testing.T, reader *metric.ManualReader, name string) (N, bool) {
	t.Helper()
	g, ok := instrument[metricdata.Gauge[N]](t, reader, name)
	if !ok || len(g.DataPoints) == 0 {
		return 0, false
	}
	return g.DataPoints[0].Value, true
}

// instrument — данные инструмента по имени из единственного scrape.
func instrument[T any](t *testing.T, reader *metric.ManualReader, name string) (T, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	var zero T
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			data, ok := m.Data.(T)
			require.True(t, ok, "инструмент %s другого типа: %T", name, m.Data)
			return data, true
		}
	}
	return zero, false
}

// newReader — ручной ридер метрик; meterOn — метр поверх него.
func newReader() *metric.ManualReader { return metric.NewManualReader() }

func meterOn(reader *metric.ManualReader) apimetric.Meter {
	return metric.NewMeterProvider(metric.WithReader(reader)).Meter("rebar.outbox")
}
