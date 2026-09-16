package inboxotel_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxotel"
	"github.com/nrect/rebar/inbox/inboxtest"
)

func TestNewObserver_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "inboxotel.NewObserver: nil meter", func() { _, _ = inboxotel.NewObserver(nil) })
}

// Отказ метра возвращается ошибкой с именем инструмента, а не роняет процесс.
func TestNewObserver_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		meter metric.Meter
		name  string
	}{
		{counterRefused{}, receivedName},
		{histogramRefused{}, durationName},
	} {
		var (
			obs inbox.Observer
			err error
		)
		assert.NotPanics(t, func() { obs, err = inboxotel.NewObserver(tc.meter) })
		require.ErrorIs(t, err, errMeter)
		assert.Contains(t, err.Error(), "инструмент "+tc.name+":")
		assert.Nil(t, obs)
	}
}

// Все пары источника × AllOutcomes есть в scrape с нуля до первой доставки: их
// заводит inbox.NewService, и первый же conflict виден increase(). Гистограмма
// нулём не заводится: записанный ноль — доставка, которой не было.
func TestObserver_WatchStartsEverySeriesAtZero(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)

	newService(obs, inboxtest.NewClock(start), nop, billing, delivery)

	ms := collect(t, reader)
	points := receivedPoints(t, ms)
	got := counts(points)
	for _, source := range []inbox.SourceName{billing, delivery} {
		for _, outcome := range inbox.AllOutcomes {
			value, ok := got[pair{source: source, outcome: outcome}]
			assert.True(t, ok, "ряда %s/%s нет до первой доставки", source, outcome)
			assert.Zero(t, value, "ряд %s/%s", source, outcome)
		}
	}
	assert.Len(t, points, 2*len(inbox.AllOutcomes), "рядов сверх пар нет")
	assertClosedLabels(t, points, billing, delivery)
	assert.Empty(t, durationPoints(t, ms), "гистограмма до первой доставки")

	obs.Watch(billing)
	assert.Len(t, receivedPoints(t, collect(t, reader)), 2*len(inbox.AllOutcomes),
		"второй сервис с тем же источником рядов не добавляет")
}

// Доставка прибавляет единицу своей паре и новых рядов не рождает: метки вне
// закрытых наборов нет ни у счётчика, ни у гистограммы.
func TestObserver_EveryPointIsFromClosedSets(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)
	obs.Watch(billing)

	for _, outcome := range inbox.AllOutcomes {
		obs.Received(t.Context(), billing, outcome, time.Second)
	}
	obs.Received(t.Context(), billing, inbox.OutcomeConflict, time.Second)

	ms := collect(t, reader)
	points := receivedPoints(t, ms)
	assert.Len(t, points, len(inbox.AllOutcomes), "доставка рядов не добавляет")
	assertClosedLabels(t, points, billing)
	got := counts(points)
	for _, outcome := range inbox.AllOutcomes {
		want := int64(1)
		if outcome == inbox.OutcomeConflict {
			want = 2
		}
		assert.Equal(t, want, got[pair{source: billing, outcome: outcome}], string(outcome))
	}

	hist := durationPoints(t, ms)
	require.Len(t, hist, 1, "у гистограммы ряд на источник")
	onlySource := attribute.NewSet(attribute.String("source", string(billing)))
	assert.True(t, hist[0].Attributes.Equals(&onlySource), "у гистограммы одна метка source, а не %v", hist[0].Attributes)
	assert.Equal(t, uint64(len(inbox.AllOutcomes)+1), hist[0].Count)
}

// Доставка, которую оборвал отправитель, приходит с отменённым ctx и обязана
// попасть и в счётчик, и в гистограмму. Ряды заведены: без них потерянный счёт
// оставил бы scrape пустым, и упал бы помощник, а не утверждение.
func TestObserver_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)
	obs.Watch(billing)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs.Received(ctx, billing, inbox.OutcomeError, time.Second)

	ms := collect(t, reader)
	assert.Equal(t, int64(1), counts(receivedPoints(t, ms))[pair{source: billing, outcome: inbox.OutcomeError}],
		"счётчик под отменённым контекстом")
	hist := durationPoints(t, ms)
	require.Len(t, hist, 1, "гистограмма под отменённым контекстом")
	assert.Equal(t, uint64(1), hist[0].Count)
}

// Время — в секундах на границах контракта: приём ровно в половину таймаута
// GitHub ложится в корзину le=5, дольше на миллисекунду — уже в le=7.5.
func TestObserver_DurationBuckets(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)

	obs.Received(t.Context(), billing, inbox.OutcomeAccepted, 5*time.Second)
	obs.Received(t.Context(), billing, inbox.OutcomeAccepted, 5*time.Second+time.Millisecond)

	hist := durationPoints(t, collect(t, reader))
	require.Len(t, hist, 1)
	dp := hist[0]
	require.Equal(t, bounds, dp.Bounds, "границы корзин — контракт алерта «обработка у таймаута»")
	assert.InDelta(t, 10.001, dp.Sum, 1e-9, "секунды, а не миллисекунды")
	at5 := slices.Index(bounds, 5)
	assert.Equal(t, uint64(1), dp.BucketCounts[at5], "ровно 5 с")
	assert.Equal(t, uint64(1), dp.BucketCounts[at5+1], "5 с и миллисекунда")
}
