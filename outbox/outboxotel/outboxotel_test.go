package outboxotel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxotel"
	"github.com/nrect/rebar/outbox/outboxtest"
)

const (
	testKind    outbox.Kind = "order.paid"
	maxAttempts             = 3
	// traceparent — заголовок породившего запроса: с ним связывается span доставки.
	traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	producerTID = "4bf92f3577b34da6a3ce929d0e0e4736"
)

var errBoom = errors.New("boom")

// Счётчик различает все семь исходов, и каждый — из закрытого набора.
func TestHandlers_CountsEveryResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		attempts int
		handler  outbox.HandlerFunc
		want     outboxotel.Result
	}{
		{
			name:    "ok",
			handler: func(context.Context, outbox.Delivery) error { return nil },
			want:    outboxotel.ResultOK,
		},
		{
			name:    "skipped — предикат не подтвердился, это не отказ",
			handler: func(context.Context, outbox.Delivery) error { return outbox.ErrSkip },
			want:    outboxotel.ResultSkipped,
		},
		{
			name:    "permanent",
			handler: func(context.Context, outbox.Delivery) error { return outbox.Permanent(errBoom) },
			want:    outboxotel.ResultPermanent,
		},
		{
			name:    "throttled — срок назван, попытка не потрачена",
			handler: func(context.Context, outbox.Delivery) error { return outbox.Throttled(errBoom, time.Minute) },
			want:    outboxotel.ResultThrottled,
		},
		{
			name:     "retry — попытки ещё есть",
			attempts: maxAttempts - 1,
			handler:  func(context.Context, outbox.Delivery) error { return errBoom },
			want:     outboxotel.ResultRetry,
		},
		{
			name:     "exhausted — попытки кончились",
			attempts: maxAttempts,
			handler:  func(context.Context, outbox.Delivery) error { return errBoom },
			want:     outboxotel.ResultExhausted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, reader, _ := newHandlers(t)

			handle(t, h, tt.handler, outbox.Delivery{Kind: testKind, Attempts: tt.attempts})

			assert.Equal(t, int64(1), counterValue(t, reader, testKind, tt.want))
		})
	}
}

// Паника считается исходом и ЛЕТИТ ДАЛЬШЕ: recover в декораторе ослепил бы
// Drain, и он не смог бы назначить строке временную ошибку.
func TestHandlers_PanicIsCountedAndRethrown(t *testing.T) {
	t.Parallel()
	h, reader, _ := newHandlers(t)
	boom := outbox.HandlerFunc(func(context.Context, outbox.Delivery) error {
		panic("хендлер взорвался")
	})

	assert.Panics(t, func() { handle(t, h, boom, outbox.Delivery{Kind: testKind}) })

	assert.Equal(t, int64(1), counterValue(t, reader, testKind, outboxotel.ResultPanic))
}

// Ошибка next уходит вызывающему как есть: классы читаются структурно, и
// обёртка, потерявшая цепочку, превратила бы постоянный отказ в вечные повторы.
func TestHandlers_PassesErrorThroughUnchanged(t *testing.T) {
	t.Parallel()
	h, _, _ := newHandlers(t)
	want := outbox.Permanent(errBoom)
	next := outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return want })

	err := h.Wrap(testKind, next).Handle(t.Context(), outbox.Delivery{Kind: testKind})

	require.ErrorIs(t, err, errBoom)
	assert.True(t, outbox.IsPermanent(err), "класс ошибки пережил декоратор")
}

// Гистограмма пишется на каждый вызов и метится только типом: result в ней не
// нужен, иначе одна метрика отвечала бы на два вопроса.
func TestHandlers_RecordsDuration(t *testing.T) {
	t.Parallel()
	h, reader, _ := newHandlers(t)

	handle(t, h, okHandler, outbox.Delivery{Kind: testKind})
	handle(t, h, okHandler, outbox.Delivery{Kind: testKind})

	hist := histogram(t, reader)
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, uint64(2), hist.DataPoints[0].Count)
	assert.Equal(t, attribute.NewSet(attribute.String("kind", string(testKind))),
		hist.DataPoints[0].Attributes)
}

// Span доставки связывается с породившим запросом ссылкой, а не родством:
// запрос давно закончился, и делать доставку его продолжением значило бы
// держать его span открытым до конца ретраев.
func TestHandlers_LinksSpanToTraceparent(t *testing.T) {
	t.Parallel()
	h, _, spans := newHandlers(t)

	handle(t, h, okHandler, outbox.Delivery{
		Kind:    testKind,
		Headers: map[string]string{"traceparent": traceparent},
	})

	ended := spans.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, "outbox.handle", ended[0].Name())
	assert.Equal(t, trace.SpanKindConsumer, ended[0].SpanKind())
	require.Len(t, ended[0].Links(), 1)
	assert.Equal(t, producerTID, ended[0].Links()[0].SpanContext.TraceID().String())
	assert.NotEqual(t, producerTID, ended[0].SpanContext().TraceID().String(), "это ссылка, а не родитель")
}

// Заголовков нет — span всё равно есть, просто без ссылки: сообщение без
// traceparent не должно оставаться невидимым.
func TestHandlers_SpanWithoutTraceparentHasNoLink(t *testing.T) {
	t.Parallel()
	h, _, spans := newHandlers(t)

	handle(t, h, okHandler, outbox.Delivery{Kind: testKind})

	ended := spans.Ended()
	require.Len(t, ended, 1)
	assert.Empty(t, ended[0].Links())
}

// Ни payload, ни идентификатор агрегата, ни текст ошибки в телеметрию не
// попадают: это данные, а не словарь.
func TestHandlers_NoDataInLabelsOrSpan(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-TOKEN-42"
	h, reader, spans := newHandlers(t)
	leaky := outbox.HandlerFunc(func(context.Context, outbox.Delivery) error {
		return errors.New("провайдер отверг " + secret)
	})

	handle(t, h, leaky, outbox.Delivery{
		Kind: testKind, AggregateID: "order-777",
		Payload: []byte(`{"token":"` + secret + `"}`), Attempts: 1,
	})

	for _, dp := range counter(t, reader).DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			assert.NotContains(t, kv.Value.AsString(), secret)
			assert.NotContains(t, kv.Value.AsString(), "order-777")
			assert.Contains(t, []string{"kind", "result"}, string(kv.Key), "лишняя метка")
		}
	}
	ended := spans.Ended()
	require.Len(t, ended, 1)
	for _, kv := range ended[0].Attributes() {
		assert.NotContains(t, kv.Value.AsString(), secret)
		assert.NotContains(t, kv.Value.AsString(), "order-777")
	}
	assert.Empty(t, ended[0].Status().Description)
}

// Обёртка не смягчает паники реестра: порядок вызовов в main не должен решать,
// какой код выполнит платёж.
func TestHandlers_RegisterKeepsRegistryPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		register func(h *outboxotel.Handlers)
	}{
		{name: "nil-хендлер", register: func(h *outboxotel.Handlers) { h.Register(testKind, nil) }},
		{name: "негодный тип", register: func(h *outboxotel.Handlers) {
			h.Register("Order Paid!", outboxtest.NewRecordingHandler())
		}},
		{name: "дубль", register: func(h *outboxotel.Handlers) {
			h.Register(testKind, outboxtest.NewRecordingHandler())
			h.Register(testKind, outboxtest.NewRecordingHandler())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _, _ := newHandlers(t)
			assert.Panics(t, func() { tt.register(h) })
		})
	}
}

// Fail closed: ошибка сборки падает на старте, а не на первом событии.
func TestNew_PanicsOnBadWiring(t *testing.T) {
	t.Parallel()
	meter := metric.NewMeterProvider().Meter("test")
	tracer := sdktrace.NewTracerProvider().Tracer("test")
	cfg := outbox.Config{MaxAttempts: maxAttempts}

	assert.Panics(t, func() { _, _ = outboxotel.New(nil, tracer, cfg) })
	assert.Panics(t, func() { _, _ = outboxotel.New(meter, nil, cfg) })
	assert.Panics(t, func() { _, _ = outboxotel.New(meter, tracer, outbox.Config{}) },
		"без MaxAttempts исход «попытки исчерпаны» неотличим от повтора")
}

// Реестр обёртки — обычный outbox.Registry: воркер собирается на нём без
// оговорок.
func TestHandlers_RegistryFeedsWorker(t *testing.T) {
	t.Parallel()
	h, reader, _ := newHandlers(t)
	recorder := outboxtest.NewRecordingHandler()
	h.Register(testKind, recorder)

	w, err := outbox.NewWorker(outboxtest.NewMemStore(), h.Registry(), workerConfig())

	require.NoError(t, err)
	assert.Equal(t, []outbox.Kind{testKind}, w.Kinds())
	assert.Empty(t, counter(t, reader).DataPoints, "без доставок счётчик молчит")
}

// okHandler — хендлер без отказов.
var okHandler = outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return nil })
