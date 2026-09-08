package schedulerotel_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulerotel"
)

// Имена и единицы — контракт для алертов потребителя, поэтому тест держит их
// литералами, а не константами пакета.
const (
	runsName        = "cron.runs"
	durationName    = "cron.duration"
	processedName   = "cron.processed"
	lastSuccessName = "cron.last_success_timestamp"

	unitRun     = "{run}"
	unitItem    = "{item}"
	unitSeconds = "s"
)

var startedAt = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func newObserver(t *testing.T) (*sdkmetric.ManualReader, scheduler.Observer) {
	t.Helper()
	reader, meter := newMeter(t)
	obs, err := schedulerotel.NewObserver(meter)
	require.NoError(t, err)
	return reader, obs
}

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return reader, provider.Meter("schedulerotel_test")
}

// collect — метрики одного scrape. Инструмент без точек данных в scrape не
// попадает вовсе, поэтому пустой результат законен: «ряда нет» — это то, что
// половина тестов и проверяет.
func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.LessOrEqual(t, len(rm.ScopeMetrics), 1)
	if len(rm.ScopeMetrics) == 0 {
		return nil
	}
	return rm.ScopeMetrics[0].Metrics
}

func metricByName(t *testing.T, ms []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()
	m, ok := findMetric(ms, name)
	if !ok {
		t.Fatalf("инструмент %q не попал в scrape", name)
	}
	return m
}

func findMetric(ms []metricdata.Metrics, name string) (metricdata.Metrics, bool) {
	for _, m := range ms {
		if m.Name == name {
			return m, true
		}
	}
	return metricdata.Metrics{}, false
}

// runsFor — значение cron.runs по паре меток; ноль, если такой пары нет.
func runsFor(t *testing.T, ms []metricdata.Metrics, job string, result schedulerotel.Result) int64 {
	t.Helper()
	sum, ok := metricByName(t, ms, runsName).Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", runsName)
	want := attribute.NewSet(
		attribute.String("job", job),
		attribute.String("result", string(result)),
	)
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

func processedFor(t *testing.T, ms []metricdata.Metrics, job string) (int64, bool) {
	t.Helper()
	m, found := findMetric(ms, processedName)
	if !found {
		return 0, false
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", processedName)
	return pointFor(sum.DataPoints, job)
}

// lastSuccessFor — значение гейджа по задаче и признак существования ряда:
// «ряда нет» — это NoData у Prometheus, и тесты проверяют именно его.
func lastSuccessFor(t *testing.T, ms []metricdata.Metrics, job string) (float64, bool) {
	t.Helper()
	m, found := findMetric(ms, lastSuccessName)
	if !found {
		return 0, false
	}
	gauge, ok := m.Data.(metricdata.Gauge[float64])
	require.True(t, ok, "%s обязан быть Float64ObservableGauge", lastSuccessName)
	return pointFor(gauge.DataPoints, job)
}

// pointFor ищет точку по метке job; сравнение по одной метке, потому что
// других у этих инструментов нет.
func pointFor[N int64 | float64](points []metricdata.DataPoint[N], job string) (N, bool) {
	for _, dp := range points {
		if name, found := dp.Attributes.Value("job"); found && name.AsString() == job {
			return dp.Value, true
		}
	}
	var zero N
	return zero, false
}

func unixSeconds(at time.Time) float64 { return float64(at.UnixNano()) / float64(time.Second) }

// run — прогон с заполненными штампами: наблюдатель их не считает, а берёт.
func run(job string, processed int, err error, panicked bool) scheduler.Run {
	return scheduler.Run{
		Job:       job,
		StartedAt: startedAt,
		Elapsed:   1500 * time.Millisecond,
		Processed: processed,
		Err:       err,
		Panicked:  panicked,
	}
}
