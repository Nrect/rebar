package idemotel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemotel"
	"github.com/nrect/rebar/idem/idemtest"
)

func TestNewObserver_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "idemotel.NewObserver: nil meter", func() { _, _ = idemotel.NewObserver(nil) })
}

// Отказ метра возвращается ошибкой, а не роняет процесс.
func TestNewObserver_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	var (
		obs idem.Observer
		err error
	)
	assert.NotPanics(t, func() { obs, err = idemotel.NewObserver(failingMeter{}) })
	require.ErrorIs(t, err, errMeter)
	assert.Nil(t, obs)
}

// Сборка хранилища заводит все пары Config.Operations × AllOutcomes нулём до
// первого Do: первый же not_recordable виден increase(). Второе хранилище на
// том же наблюдателе рядов не удваивает.
func TestObserver_WatchStartsEverySeriesAtZero(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := idemotel.NewObserver(meter)
	require.NoError(t, err)

	idemtest.NewMemStore(testConfig(), obs)
	idemtest.NewMemStore(testConfig(), obs)

	points := requestPoints(t, reader)
	assert.Len(t, points, len(operations)*len(idem.AllOutcomes), "рядов после сборки")
	assertClosedLabels(t, points)
	for _, op := range operations {
		for _, outcome := range idem.AllOutcomes {
			value, found := requestCount(points, op, outcome)
			assert.True(t, found, "ряд %s/%s не заведён при сборке хранилища", op, outcome)
			assert.Zero(t, value, "ряд %s/%s", op, outcome)
		}
	}
}

// Исход прибавляет единицу ровно своей паре и новых рядов не рождает: метка
// вне закрытых наборов не появляется.
func TestObserver_EveryPointIsFromClosedSets(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := idemotel.NewObserver(meter)
	require.NoError(t, err)
	for _, op := range operations {
		obs.Watch(op)
	}

	for _, outcome := range idem.AllOutcomes {
		obs.Outcome(t.Context(), opCreate, outcome)
	}
	obs.Outcome(t.Context(), opUpdate, idem.OutcomeReused)
	obs.Outcome(t.Context(), opUpdate, idem.OutcomeReused)

	points := requestPoints(t, reader)
	assert.Len(t, points, len(operations)*len(idem.AllOutcomes), "исход рядов не добавляет")
	assertClosedLabels(t, points)
	for _, outcome := range idem.AllOutcomes {
		want := map[idem.Operation]int64{opCreate: 1}
		if outcome == idem.OutcomeReused {
			want[opUpdate] = 2
		}
		for _, op := range operations {
			value, _ := requestCount(points, op, outcome)
			assert.Equal(t, want[op], value, "ряд %s/%s", op, outcome)
		}
	}
}

// Do, оборванный отменой, отдаёт исход с уже отменённым ctx: он обязан попасть
// в счётчик всё равно.
func TestObserver_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := idemotel.NewObserver(meter)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs.Outcome(ctx, opCreate, idem.OutcomeError)

	value, _ := requestCount(requestPoints(t, reader), opCreate, idem.OutcomeError)
	assert.Equal(t, int64(1), value)
}
