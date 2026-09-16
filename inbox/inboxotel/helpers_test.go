package inboxotel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxotel"
	"github.com/nrect/rebar/inbox/inboxtest"
)

// Имена, единицы и границы корзин — контракт для алертов потребителя: тест
// держит их литералами.
const (
	receivedName = "inbox_received"
	durationName = "inbox_receive_duration"
	unitDelivery = "{delivery}"
	unitSeconds  = "s"
)

var bounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 15, 30}

const (
	billing   inbox.SourceName = "billing"
	delivery  inbox.SourceName = "delivery"
	typePaid  inbox.EventType  = "invoice.paid"
	typeDraft inbox.EventType  = "invoice.draft"
	maxBody                    = 4096
)

var (
	start   = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	secret  = []byte("inboxotel-test-secret-0123456789")
	errDown = errors.New("inboxotel_test: store is down")
	nop     = inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return nil })
)

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })
	return reader, mp.Meter("rebar.inbox")
}

func newObserver(t *testing.T) (*sdkmetric.ManualReader, inbox.Observer) {
	t.Helper()
	reader, meter := newMeter(t)
	obs, err := inboxotel.NewObserver(meter)
	require.NoError(t, err)
	return reader, obs
}

// newService — сервис на двойнике и управляемых часах: у каждого источника
// тестовая подпись, Handle — typePaid, Ignore — typeDraft, обработчик один.
func newService(obs inbox.Observer, clock *inboxtest.Clock, handler inboxtest.Handler, sources ...inbox.SourceName,
) (*inbox.Service, *inboxtest.MemStore) {
	handlers := make(map[inbox.SourceName]inboxtest.Handler, len(sources))
	cfg := inbox.Config{
		Sources:      make(map[inbox.SourceName]inbox.SourceConfig, len(sources)),
		MaxBodyBytes: maxBody, Retention: 30 * 24 * time.Hour, PayloadRetention: 72 * time.Hour, PurgeBatch: 100,
	}
	for _, name := range sources {
		handlers[name] = handler
		cfg.Sources[name] = inbox.SourceConfig{
			Verifier: inboxtest.NewHMACVerifier(name, 5*time.Minute, clock.Now, secret),
			Handle:   []inbox.EventType{typePaid},
			Ignore:   []inbox.EventType{typeDraft},
		}
	}
	store := inboxtest.NewMemStore(handlers)
	svc := inbox.NewService(store, obs, cfg)
	svc.SetClock(clock.Now)
	return svc, store
}

func signed(id string, typ inbox.EventType, data any) inbox.Request {
	return inboxtest.SignHMAC(secret, start, inboxtest.EventBody(id, typ, data))
}

// collect — метрики одного scrape.
func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	return rm.ScopeMetrics[0].Metrics
}

// receivedPoints — точки счётчика доставок; имя, единица и монотонность — контракт.
func receivedPoints(t *testing.T, ms []metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, m := range ms {
		if m.Name != receivedName {
			continue
		}
		require.Equal(t, unitDelivery, m.Unit)
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s обязан быть Int64Counter", receivedName)
		require.True(t, sum.IsMonotonic, "%s обязан быть счётчиком, а не UpDown", receivedName)
		return sum.DataPoints
	}
	require.Fail(t, "инструмента нет в scrape", receivedName)
	return nil
}

// durationPoints — точки гистограммы времени приёма; nil — наблюдений не было.
func durationPoints(t *testing.T, ms []metricdata.Metrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, m := range ms {
		if m.Name != durationName {
			continue
		}
		require.Equal(t, unitSeconds, m.Unit)
		hist, ok := m.Data.(metricdata.Histogram[float64])
		require.True(t, ok, "%s обязан быть Float64Histogram", durationName)
		return hist.DataPoints
	}
	return nil
}

// pair — ряд счётчика: источник и исход.
type pair struct {
	source  inbox.SourceName
	outcome inbox.Outcome
}

// counts — значения рядов счётчика по паре меток.
func counts(points []metricdata.DataPoint[int64]) map[pair]int64 {
	got := make(map[pair]int64, len(points))
	for _, dp := range points {
		source, _ := dp.Attributes.Value("source")
		outcome, _ := dp.Attributes.Value("outcome")
		got[pair{source: inbox.SourceName(source.AsString()), outcome: inbox.Outcome(outcome.AsString())}] = dp.Value
	}
	return got
}

// assertClosedLabels — у каждой точки счётчика ровно source и outcome, оба из
// закрытых наборов.
func assertClosedLabels(t *testing.T, points []metricdata.DataPoint[int64], sources ...inbox.SourceName) {
	t.Helper()
	for _, dp := range points {
		require.Equal(t, 2, dp.Attributes.Len(), "меток ровно две: source и outcome")
		source, ok := dp.Attributes.Value("source")
		require.True(t, ok, "у точки нет метки source")
		outcome, ok := dp.Attributes.Value("outcome")
		require.True(t, ok, "у точки нет метки outcome")
		assert.Contains(t, sources, inbox.SourceName(source.AsString()), "метка source вне Config")
		assert.Contains(t, inbox.AllOutcomes, inbox.Outcome(outcome.AsString()), "метка outcome вне inbox.AllOutcomes")
	}
}

var errMeter = errors.New("meter: instrument refused")

// counterRefused — метр, отказывающий в счётчике.
type counterRefused struct{ noop.Meter }

func (counterRefused) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errMeter
}

// histogramRefused — метр, отказывающий в гистограмме.
type histogramRefused struct{ noop.Meter }

func (histogramRefused) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errMeter
}
