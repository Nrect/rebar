package paymentotel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
)

// Имена и единицы инструментов — контракт для алертов потребителя, поэтому
// тест держит их литералами, а не константами пакета.
const (
	callsName    = "payment_provider_calls"
	outcomesName = "payments"
	stuckName    = "payment_intents_stuck"
	driftName    = "payment_drift"

	unitCall      = "{call}"
	unitOperation = "{operation}"
	unitIntent    = "{intent}"
)

// noKind — ключ ряда расхождений без метки: род вне закрытого набора.
const noKind = "<без kind>"

// markerKey — по значению под этим ключом видно, что до next дошёл контекст
// вызывающего, а не новый.
type markerKey struct{}

// seenCall — что дошло до next: метод, значение из контекста и аргументы.
type seenCall struct {
	method string
	marker any
	args   []any
}

// stubProvider — локальный двойник порта с подставным исходом:
// paymenttest.MemProvider не запоминает аргументов ParseWebhook и GetPayment, а
// декоратору нужно видеть, что именно дошло до next.
type stubProvider struct {
	name  payment.ProviderName
	err   error                       // исход любого вызова
	res   payment.CreatePaymentResult // ответ CreatePayment
	ev    payment.Event               // ответ остальных методов
	calls []seenCall
	names int // сколько раз звали Name
}

func newStub(err error) *stubProvider { return &stubProvider{name: "stub", err: err} }

func (s *stubProvider) Name() payment.ProviderName {
	s.names++
	return s.name
}

func (s *stubProvider) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest,
) (payment.CreatePaymentResult, error) {
	s.record(ctx, "CreatePayment", req)
	return s.res, s.err
}

func (s *stubProvider) ParseWebhook(ctx context.Context, req payment.WebhookRequest) (payment.Event, error) {
	s.record(ctx, "ParseWebhook", req)
	return s.ev, s.err
}

func (s *stubProvider) GetPayment(ctx context.Context, providerPaymentID string) (payment.Event, error) {
	s.record(ctx, "GetPayment", providerPaymentID)
	return s.ev, s.err
}

func (s *stubProvider) Capture(ctx context.Context, req payment.CaptureRequest) (payment.Event, error) {
	s.record(ctx, "Capture", req)
	return s.ev, s.err
}

func (s *stubProvider) Cancel(ctx context.Context, providerPaymentID, idempotencyKey string) (payment.Event, error) {
	s.record(ctx, "Cancel", providerPaymentID, idempotencyKey)
	return s.ev, s.err
}

func (s *stubProvider) Refund(ctx context.Context, req payment.RefundProviderRequest) (payment.Event, error) {
	s.record(ctx, "Refund", req)
	return s.ev, s.err
}

func (s *stubProvider) record(ctx context.Context, method string, args ...any) {
	s.calls = append(s.calls, seenCall{method: method, marker: ctx.Value(markerKey{}), args: args})
}

// portCall — метод порта одним вызовом и type, которым он обязан посчитаться.
type portCall struct {
	typ paymentotel.CallType
	do  func(ctx context.Context, p payment.Provider) error
}

// everyCall — все считаемые методы порта; Name сюда не входит, он не считается.
func everyCall() []portCall {
	return []portCall{
		{paymentotel.CallCreatePayment, func(ctx context.Context, p payment.Provider) error {
			_, err := p.CreatePayment(ctx, payment.CreatePaymentRequest{})
			return err
		}},
		{paymentotel.CallParseWebhook, func(ctx context.Context, p payment.Provider) error {
			_, err := p.ParseWebhook(ctx, payment.WebhookRequest{})
			return err
		}},
		{paymentotel.CallGetPayment, func(ctx context.Context, p payment.Provider) error {
			_, err := p.GetPayment(ctx, "pay-1")
			return err
		}},
		{paymentotel.CallCapture, func(ctx context.Context, p payment.Provider) error {
			_, err := p.Capture(ctx, payment.CaptureRequest{})
			return err
		}},
		{paymentotel.CallCancel, func(ctx context.Context, p payment.Provider) error {
			_, err := p.Cancel(ctx, "pay-1", "key-1")
			return err
		}},
		{paymentotel.CallRefund, func(ctx context.Context, p payment.Provider) error {
			_, err := p.Refund(ctx, payment.RefundProviderRequest{})
			return err
		}},
	}
}

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })
	return reader, mp.Meter("rebar.payment")
}

// wrap — декоратор над next на свежем метре.
func wrap(t *testing.T, next payment.Provider) (payment.Provider, *sdkmetric.ManualReader) {
	t.Helper()
	reader, meter := newMeter(t)
	p, err := paymentotel.Wrap(next, meter)
	require.NoError(t, err)
	return p, reader
}

func newGauges(t *testing.T) (*paymentotel.Gauges, *sdkmetric.ManualReader) {
	t.Helper()
	reader, meter := newMeter(t)
	g, err := paymentotel.NewGauges(meter)
	require.NoError(t, err)
	return g, reader
}

// collect — метрики одного scrape; nil, если в нём нет ни одной.
func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	if len(rm.ScopeMetrics) == 0 {
		return nil
	}
	require.Len(t, rm.ScopeMetrics, 1)
	return rm.ScopeMetrics[0].Metrics
}

// findMetric — инструмент по имени; ok = false, если в scrape его нет.
func findMetric(ms []metricdata.Metrics, name string) (metricdata.Metrics, bool) {
	for _, m := range ms {
		if m.Name == name {
			return m, true
		}
	}
	return metricdata.Metrics{}, false
}

func metricByName(t *testing.T, ms []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()
	m, ok := findMetric(ms, name)
	require.True(t, ok, "инструмента %q в scrape нет", name)
	return m
}

// callPoints — точки счётчика вызовов; пусто, если вызовов не было.
func callPoints(t *testing.T, ms []metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	m, ok := findMetric(ms, callsName)
	if !ok {
		return nil
	}
	require.Equal(t, unitCall, m.Unit)
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", callsName)
	require.True(t, sum.IsMonotonic, "%s обязан быть счётчиком, а не UpDown", callsName)
	return sum.DataPoints
}

// callCount — вызовы по type и result, по всем провайдерам; ноль, если таких нет.
func callCount(t *testing.T, ms []metricdata.Metrics, typ paymentotel.CallType, result paymentotel.Result) int64 {
	t.Helper()
	var total int64
	for _, dp := range callPoints(t, ms) {
		gotType, _ := dp.Attributes.Value("type")
		gotResult, _ := dp.Attributes.Value("result")
		if gotType.AsString() == string(typ) && gotResult.AsString() == string(result) {
			total += dp.Value
		}
	}
	return total
}

// callCountOf — вызовы одного провайдера по type и result.
func callCountOf(t *testing.T, ms []metricdata.Metrics, provider string,
	typ paymentotel.CallType, result paymentotel.Result,
) int64 {
	t.Helper()
	want := attribute.NewSet(
		attribute.String("provider", provider),
		attribute.String("type", string(typ)),
		attribute.String("result", string(result)),
	)
	for _, dp := range callPoints(t, ms) {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

// outcomePoints — точки счётчика исходов; пусто, если наблюдателя не собирали.
func outcomePoints(t *testing.T, ms []metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	m, ok := findMetric(ms, outcomesName)
	if !ok {
		return nil
	}
	require.Equal(t, unitOperation, m.Unit)
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", outcomesName)
	require.True(t, sum.IsMonotonic, "%s обязан быть счётчиком, а не UpDown", outcomesName)
	return sum.DataPoints
}

// outcomeCount — значение счётчика исходов по паре меток; ноль, если пары нет.
func outcomeCount(t *testing.T, ms []metricdata.Metrics, op payment.Op, reason payment.Reason) int64 {
	t.Helper()
	want := attribute.NewSet(
		attribute.String("op", string(op)),
		attribute.String("reason", string(reason)),
	)
	for _, dp := range outcomePoints(t, ms) {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

// gaugePoints — точки int64-гейджа по имени.
func gaugePoints(t *testing.T, ms []metricdata.Metrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	g, ok := metricByName(t, ms, name).Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s обязан быть Int64ObservableGauge", name)
	return g.DataPoints
}

func stuck(t *testing.T, ms []metricdata.Metrics) int64 {
	t.Helper()
	points := gaugePoints(t, ms, stuckName)
	require.Len(t, points, 1)
	return points[0].Value
}

// driftByKind — точки payment_drift по метке kind; ряд без меток — под ключом
// noKind.
func driftByKind(t *testing.T, ms []metricdata.Metrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, dp := range gaugePoints(t, ms, driftName) {
		key := noKind
		if v, ok := dp.Attributes.Value("kind"); ok {
			require.Equal(t, 1, dp.Attributes.Len(), "у ряда расхождений метка одна — kind")
			key = v.AsString()
		} else {
			require.Zero(t, dp.Attributes.Len(), "ряд без kind обязан быть без меток вовсе")
		}
		_, dup := out[key]
		require.False(t, dup, "ряд %q в scrape дважды", key)
		out[key] = dp.Value
	}
	return out
}

// drifts — n записей рода kind.
func drifts(kind payment.DriftKind, n int) []payment.DriftRecord {
	out := make([]payment.DriftRecord, 0, n)
	for range n {
		out = append(out, payment.DriftRecord{Reference: "order-1", Kind: kind})
	}
	return out
}

var errMeter = errors.New("meter: instrument refused")

// failCallback — failOn, при котором метр отказывает в регистрации коллбэка.
const failCallback = "<callback>"

// failingMeter — метр, отказывающий в инструменте с именем failOn либо в
// регистрации коллбэка: так видно, что ошибка сборки возвращается, а не роняет
// процесс.
type failingMeter struct {
	noop.Meter
	failOn string
}

func (m failingMeter) Int64Counter(name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	if name == m.failOn {
		return nil, errMeter
	}
	return m.Meter.Int64Counter(name, opts...)
}

func (m failingMeter) Int64ObservableGauge(name string, opts ...metric.Int64ObservableGaugeOption,
) (metric.Int64ObservableGauge, error) {
	if name == m.failOn {
		return nil, errMeter
	}
	return m.Meter.Int64ObservableGauge(name, opts...)
}

func (m failingMeter) RegisterCallback(f metric.Callback, instruments ...metric.Observable,
) (metric.Registration, error) {
	if m.failOn == failCallback {
		return nil, errMeter
	}
	return m.Meter.RegisterCallback(f, instruments...)
}
