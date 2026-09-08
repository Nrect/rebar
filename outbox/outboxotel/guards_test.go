package outboxotel_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxotel"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// Набор result ЗАКРЫТ: на нём стоят алерты потребителя, и метка, которой в
// AllResults нет, означала бы дашборд, собранный по строке из кода.
func TestAllResults_IsClosedSet(t *testing.T) {
	t.Parallel()

	assert.Len(t, outboxotel.AllResults, 7, "исходов ровно семь (ADR-0002, «Наблюдаемость»)")
	seen := map[outboxotel.Result]bool{}
	for _, r := range outboxotel.AllResults {
		assert.NotEmpty(t, r, "пустая метка — это отсутствие метки")
		assert.False(t, seen[r], "исход %s перечислен дважды", r)
		seen[r] = true
	}
	for _, r := range []outboxotel.Result{
		outboxotel.ResultOK, outboxotel.ResultSkipped, outboxotel.ResultRetry,
		outboxotel.ResultThrottled, outboxotel.ResultPermanent,
		outboxotel.ResultExhausted, outboxotel.ResultPanic,
	} {
		assert.Contains(t, outboxotel.AllResults, r)
	}
}

// Классификация не умеет отдать значение вне закрытого набора: страж ловит
// исход, добавленный в код и забытый в AllResults.
func TestHandlers_EveryObservedResultIsInAllResults(t *testing.T) {
	t.Parallel()
	h, reader, _ := newHandlers(t)

	for _, next := range []outbox.Handler{
		okHandler,
		outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return outbox.ErrSkip }),
		outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return outbox.Permanent(errBoom) }),
		outbox.HandlerFunc(func(context.Context, outbox.Delivery) error {
			return outbox.Throttled(errBoom, time.Minute)
		}),
		outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return errBoom }),
	} {
		handle(t, h, next, outbox.Delivery{Kind: testKind, Attempts: 1})
		handle(t, h, next, outbox.Delivery{Kind: testKind, Attempts: maxAttempts})
	}
	assert.Panics(t, func() {
		handle(t, h, outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { panic("boom") }),
			outbox.Delivery{Kind: testKind})
	})

	points := counter(t, reader).DataPoints
	require.NotEmpty(t, points)
	for _, dp := range points {
		value, ok := dp.Attributes.Value("result")
		require.True(t, ok, "у точки нет метки result")
		assert.Contains(t, outboxotel.AllResults, outboxotel.Result(value.AsString()),
			"исход %s не объявлен в AllResults", value.AsString())
	}
	assert.Len(t, points, len(outboxotel.AllResults), "все семь исходов достижимы")
}

// Register кладёт в реестр ИМЕННО обёртку: иначе Wrap был бы зелёным, а прод
// считал бы ноль.
func TestHandlers_RegisteredHandlerIsInstrumented(t *testing.T) {
	t.Parallel()
	h, reader, spans := newHandlers(t)
	recorder := outboxtest.NewRecordingHandler()
	h.Register(testKind, recorder)

	store := outboxtest.NewMemStore()
	w, err := outbox.NewWorker(store, h.Registry(), workerConfig())
	require.NoError(t, err)
	clock := outboxtest.NewClock(time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC))
	w.SetClock(clock.Now)
	_, err = outboxtest.Enqueue(t.Context(), store, deliverable(clock.Now()))
	require.NoError(t, err)

	processed, err := w.Drain(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Len(t, recorder.Handled(), 1)
	assert.Equal(t, int64(1), counterValue(t, reader, testKind, outboxotel.ResultOK),
		"доставка через воркер посчитана")
	assert.Len(t, spans.Ended(), 1)
}

// Gauges отдают ровно то, что дал снимок, и до первого Set — нули.
func TestGauges_ObserveSnapshot(t *testing.T) {
	t.Parallel()
	reader := newReader()
	g, err := outboxotel.NewGauges(meterOn(reader))
	require.NoError(t, err)

	for _, name := range []string{"outbox_pending", "outbox_processing", "outbox_failed", "outbox_unhandled"} {
		value, ok := gaugeValue[int64](t, reader, name)
		require.True(t, ok, "гейджа %s нет в scrape", name)
		assert.Zero(t, value, "%s до первого Set", name)
	}
	age, ok := gaugeValue[float64](t, reader, "outbox_oldest_due_age")
	require.True(t, ok)
	assert.Zero(t, age)

	g.Set(outbox.Stats{
		Pending: 7, Processing: 2, Failed: 3, Unhandled: 1, OldestDueAge: 90 * time.Second,
	})

	for name, want := range map[string]int64{
		"outbox_pending": 7, "outbox_processing": 2, "outbox_failed": 3, "outbox_unhandled": 1,
	} {
		value, found := gaugeValue[int64](t, reader, name)
		require.True(t, found)
		assert.Equal(t, want, value, name)
	}
	age, ok = gaugeValue[float64](t, reader, "outbox_oldest_due_age")
	require.True(t, ok)
	assert.InDelta(t, 90.0, age, 0.001, "возраст в секундах: экспортёр припишет _seconds")
}

// Снимок читается один раз на scrape: «pending» и «возраст» на дашборде
// обязаны быть из одного момента.
func TestGauges_UnregisterStopsScrape(t *testing.T) {
	t.Parallel()
	reader := newReader()
	g, err := outboxotel.NewGauges(meterOn(reader))
	require.NoError(t, err)
	g.Set(outbox.Stats{Pending: 5})

	require.NoError(t, g.Unregister())
	g.Set(outbox.Stats{Pending: 9}) // после Unregister безвреден

	_, ok := gaugeValue[int64](t, reader, "outbox_pending")
	assert.False(t, ok, "снятый коллбэк в scrape не попадает")
}

func TestNewGauges_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { _, _ = outboxotel.NewGauges(nil) })
}

// deliverable — конверт, готовый к захвату воркером.
func deliverable(now time.Time) outbox.Envelope {
	id := uuid.New()
	return outbox.Envelope{
		ID: id, Kind: testKind, Payload: []byte(`{"order":42}`),
		DedupKey: "order:" + id.String(), SchemaVersion: 1,
		Headers:     map[string]string{"traceparent": traceparent},
		Fingerprint: []byte{0x01, 0x02, 0x03},
		Status:      outbox.StatusPending,
		AvailableAt: now, OccurredAt: now, CreatedAt: now, UpdatedAt: now,
	}
}
