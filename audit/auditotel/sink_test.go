package auditotel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditotel"
	"github.com/nrect/rebar/audit/audittest"
)

// Имя и единица инструмента — контракт для алертов потребителя, поэтому тест
// держит их литералами, а не константами пакета.
const (
	counterName = "audit_events"
	unitEvent   = "{event}"
)

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return reader, provider.Meter("auditotel_test")
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	require.Len(t, rm.ScopeMetrics[0].Metrics, 1, "инструмент в пакете ровно один")
	return rm.ScopeMetrics[0].Metrics[0]
}

// counterValue — значение audit_events по паре меток; ноль, если пары нет.
func counterValue(t *testing.T, m metricdata.Metrics, action audit.Action, outcome audit.Outcome) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s обязан быть Int64Counter", m.Name)
	want := attribute.NewSet(
		attribute.String("action", string(action)),
		attribute.String("outcome", string(outcome)),
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

func event(action audit.Action, outcome audit.Outcome) audit.Event {
	return audit.Event{
		ID:      uuid.New(),
		Action:  action,
		Outcome: outcome,
		Actor:   audit.Actor{Kind: audit.ActorUser, ID: "u-1", Name: "teacher@school.ru"},
		Target:  audit.Target{Type: "user", ID: "u-1"},
		IP:      "203.0.113.7",
		Details: map[string]string{"reason": "bad_password"},
	}
}

// Nil-порт и nil-метр — паника на старте, как у audit.NewRecorder.
func TestWrap_PanicsOnNil(t *testing.T) {
	t.Parallel()

	_, meter := newMeter(t)
	assert.PanicsWithValue(t, "auditotel.Wrap: nil sink", func() { _, _ = auditotel.Wrap(nil, meter) })
	assert.PanicsWithValue(t, "auditotel.Wrap: nil meter", func() { _, _ = auditotel.Wrap(audittest.NewSink(), nil) })
}

// Имя и единица — контракт: на них стоят алерты потребителя.
func TestWrap_InstrumentNameAndUnit(t *testing.T) {
	t.Parallel()

	reader, meter := newMeter(t)
	sink, err := auditotel.Wrap(audittest.NewSink(), meter)
	require.NoError(t, err)
	require.NoError(t, sink.Write(t.Context(), event("user.login", audit.OutcomeSuccess)))

	m := collect(t, reader)
	assert.Equal(t, counterName, m.Name)
	assert.Equal(t, unitEvent, m.Unit)
}

// Счётчик различает действие и исход — и только их.
func TestSink_CountsByActionAndOutcome(t *testing.T) {
	t.Parallel()

	reader, meter := newMeter(t)
	inner := audittest.NewSink()
	sink, err := auditotel.Wrap(inner, meter)
	require.NoError(t, err)

	for _, ev := range []audit.Event{
		event("user.login", audit.OutcomeSuccess),
		event("user.login", audit.OutcomeSuccess),
		event("user.login", audit.OutcomeDenied),
		event("order.refund", audit.OutcomeFailure),
	} {
		require.NoError(t, sink.Write(t.Context(), ev))
	}

	m := collect(t, reader)
	assert.Equal(t, int64(2), counterValue(t, m, "user.login", audit.OutcomeSuccess))
	assert.Equal(t, int64(1), counterValue(t, m, "user.login", audit.OutcomeDenied))
	assert.Equal(t, int64(1), counterValue(t, m, "order.refund", audit.OutcomeFailure))
	assert.Equal(t, 3, counterPoints(t, m), "рядов ровно столько, сколько пар меток")
	assert.Equal(t, 4, inner.Count(), "декоратор пропускает все события вложенному")
}

// В метки не попадает ничего, кроме действия и исхода: адрес и логин в системе
// мониторинга — персональные данные, которые оттуда уже не удалить.
func TestSink_LabelsAreOnlyActionAndOutcome(t *testing.T) {
	t.Parallel()

	reader, meter := newMeter(t)
	sink, err := auditotel.Wrap(audittest.NewSink(), meter)
	require.NoError(t, err)
	ev := event("user.login", audit.OutcomeDenied)
	require.NoError(t, sink.Write(t.Context(), ev))

	sum, ok := collect(t, reader).Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)

	keys := sum.DataPoints[0].Attributes.ToSlice()
	require.Len(t, keys, 2)
	for _, kv := range keys {
		assert.Contains(t, []attribute.Key{"action", "outcome"}, kv.Key)
		assert.NotContains(t, kv.Value.AsString(), ev.Actor.Name)
		assert.NotContains(t, kv.Value.AsString(), ev.IP)
	}
}

// Незаписанное событие не считается: иначе на дашборде были бы события,
// которых в журнале нет (doc.go, п. 3).
func TestSink_DoesNotCountFailedWrite(t *testing.T) {
	t.Parallel()

	reader, meter := newMeter(t)
	inner := audittest.NewSink()
	inner.Err = audittest.ErrSinkFailed
	sink, err := auditotel.Wrap(inner, meter)
	require.NoError(t, err)

	writeErr := sink.Write(t.Context(), event("user.login", audit.OutcomeSuccess))
	require.ErrorIs(t, writeErr, audittest.ErrSinkFailed, "ошибка вложенного проходит без изменений")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.Empty(t, rm.ScopeMetrics, "ни одного ряда: писать было нечего")
}

// Ошибка вложенного приёмника доезжает до вызывающего целиком: цепочка
// errors.Is обязана пережить декоратор.
func TestSink_PassesErrorChainThrough(t *testing.T) {
	t.Parallel()

	_, meter := newMeter(t)
	inner := audittest.NewSink()
	inner.Err = errors.Join(audit.ErrUnavailable, audittest.ErrSinkFailed)
	sink, err := auditotel.Wrap(inner, meter)
	require.NoError(t, err)

	writeErr := sink.Write(t.Context(), event("user.login", audit.OutcomeSuccess))
	require.ErrorIs(t, writeErr, audit.ErrUnavailable)
	require.ErrorIs(t, writeErr, audittest.ErrSinkFailed)
}

// Декоратор подставляется в ядро как обычный порт: сборка потребителя не
// узнаёт о метриках ничего.
func TestSink_WorksAsRecorderPort(t *testing.T) {
	t.Parallel()

	reader, meter := newMeter(t)
	inner := audittest.NewSink()
	sink, err := auditotel.Wrap(inner, meter)
	require.NoError(t, err)

	rec := audit.NewRecorder(sink, audit.Config{
		Actions:      []audit.Action{"user.login"},
		MaxDetails:   4,
		MaxDetailLen: 32,
	})
	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorAnonymous})
	require.NoError(t, rec.Record(ctx, audit.Entry{Action: "user.login", Outcome: audit.OutcomeDenied}))

	assert.Equal(t, int64(1), counterValue(t, collect(t, reader), "user.login", audit.OutcomeDenied))
	assert.Equal(t, 1, inner.Count())
}
