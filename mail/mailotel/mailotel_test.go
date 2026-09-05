package mailotel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailotel"
)

// Имена инструментов — контракт для алертов потребителя, поэтому тест держит
// их литералами, а не константами пакета.
const (
	counterName = "emails_sent"
	pendingName = "email_outbox_pending"
	oldestName  = "email_outbox_oldest_pending_age"
	failedName  = "email_outbox_failed"

	unitEmail   = "{email}"
	unitSeconds = "s"
)

// stubTransport — локальный двойник порта: mailtest.Transport пишет другая
// сессия, а декоратору хватает подставного исхода.
type stubTransport struct {
	name mail.TransportName
	res  mail.SendResult
	err  error
	last mail.Envelope
}

func newStub(err error) *stubTransport { return &stubTransport{name: "stub", err: err} }

func (s *stubTransport) Name() mail.TransportName { return s.name }

func (s *stubTransport) Send(_ context.Context, env mail.Envelope) (mail.SendResult, error) {
	s.last = env
	return s.res, s.err
}

func envelope(kind mail.Kind) mail.Envelope {
	return mail.Envelope{
		Kind:    kind,
		To:      mail.Address{Email: "teacher@school.ru"},
		Subject: "Подтверждение почты",
		Text:    "Ссылка: https://example.ru/verify?token=abc",
	}
}

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return reader, provider.Meter("mailotel_test")
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	return rm.ScopeMetrics[0].Metrics
}

func metricByName(t *testing.T, ms []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("инструмент %q не создан", name)
	return metricdata.Metrics{}
}

// counterValue — значение emails_sent по паре меток; ноль, если такой пары нет.
func counterValue(t *testing.T, m metricdata.Metrics, kind string, result mailotel.Result) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", m.Name)
	want := attribute.NewSet(
		attribute.String("type", kind),
		attribute.String("result", string(result)),
	)
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

func counterPoints(t *testing.T, m metricdata.Metrics) int {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", m.Name)
	return len(sum.DataPoints)
}

func gaugeInt64(t *testing.T, ms []metricdata.Metrics, name string) int64 {
	t.Helper()
	g, ok := metricByName(t, ms, name).Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s обязан быть Int64ObservableGauge", name)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

// oldestAge — единственный гейдж с плавающей точкой, поэтому имя не параметр.
func oldestAge(t *testing.T, ms []metricdata.Metrics) float64 {
	t.Helper()
	g, ok := metricByName(t, ms, oldestName).Data.(metricdata.Gauge[float64])
	require.True(t, ok, "%s обязан быть Float64ObservableGauge", oldestName)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

// Набор result закрыт: значения — метки, по которым у потребителя стоят
// алерты, и смена строки ломающая (VERSIONING).
func TestAllResultsIsComplete(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		[]mailotel.Result{mailotel.ResultOK, mailotel.ResultRejected, mailotel.ResultError},
		mailotel.AllResults)
	assert.Equal(t, "ok", string(mailotel.ResultOK))
	assert.Equal(t, "rejected", string(mailotel.ResultRejected))
	assert.Equal(t, "error", string(mailotel.ResultError))
}

// Nil-порт и nil-метр — паника в конструкторе, как у mail.NewService: ошибка
// сборки обязана падать на старте.
func TestConstructors_PanicOnNil(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)

	assert.Panics(t, func() { _, _ = mailotel.Wrap(nil, meter) })
	assert.Panics(t, func() { _, _ = mailotel.Wrap(newStub(nil), nil) })
	assert.Panics(t, func() { _, _ = mailotel.NewGauges(nil) })

	assert.NotPanics(t, func() {
		_, err := mailotel.Wrap(newStub(nil), meter)
		require.NoError(t, err)
	})
}
