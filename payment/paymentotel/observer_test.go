package paymentotel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
)

// Все пары AllOps × AllReasons есть в scrape с нуля, других нет: первый же
// status_conflict виден increase(), а метка вне закрытых наборов не рождается.
func TestNewObserver_EverySeriesStartsAtZero(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	_, err := paymentotel.NewObserver(meter)
	require.NoError(t, err)

	points := outcomePoints(t, collect(t, reader))

	assert.Len(t, points, len(payment.AllOps)*len(payment.AllReasons))
	for _, dp := range points {
		assert.Zero(t, dp.Value)
		require.Equal(t, 2, dp.Attributes.Len(), "меток ровно две: op и reason")
		op, ok := dp.Attributes.Value("op")
		require.True(t, ok, "у точки нет метки op")
		reason, ok := dp.Attributes.Value("reason")
		require.True(t, ok, "у точки нет метки reason")
		assert.Contains(t, payment.AllOps, payment.Op(op.AsString()))
		assert.Contains(t, payment.AllReasons, payment.Reason(reason.AsString()))
	}
}

// Outcome прибавляет единицу ровно своей паре.
func TestObserver_CountsOutcome(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := paymentotel.NewObserver(meter)
	require.NoError(t, err)

	obs.Outcome(t.Context(), payment.OpWebhook, payment.ReasonStatusConflict)
	obs.Outcome(t.Context(), payment.OpWebhook, payment.ReasonStatusConflict)
	obs.Outcome(t.Context(), payment.OpStart, payment.ReasonCreated)

	ms := collect(t, reader)
	assert.Equal(t, int64(2), outcomeCount(t, ms, payment.OpWebhook, payment.ReasonStatusConflict))
	assert.Equal(t, int64(1), outcomeCount(t, ms, payment.OpStart, payment.ReasonCreated))
	assert.Zero(t, outcomeCount(t, ms, payment.OpStart, payment.ReasonStatusConflict))
}

// Операция, упавшая по таймауту, зовёт наблюдателя с уже отменённым ctx: исход
// обязан попасть в счётчик всё равно.
func TestObserver_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := paymentotel.NewObserver(meter)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs.Outcome(ctx, payment.OpWebhook, payment.ReasonProviderError)

	assert.Equal(t, int64(1), outcomeCount(t, collect(t, reader), payment.OpWebhook, payment.ReasonProviderError))
}

func TestNewObserver_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { _, _ = paymentotel.NewObserver(nil) })
}

// Отказ метра возвращается ошибкой, а не роняет процесс.
func TestNewObserver_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	var (
		obs payment.Observer
		err error
	)

	assert.NotPanics(t, func() { obs, err = paymentotel.NewObserver(failingMeter{failOn: outcomesName}) })

	require.ErrorIs(t, err, errMeter)
	assert.Nil(t, obs)
}
