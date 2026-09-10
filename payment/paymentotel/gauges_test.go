package paymentotel_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
)

// До первого Set гейджи отдают нули, а не отсутствуют: пустой ряд на дашборде
// неотличим от «экспортёр отвалился».
func TestGauges_ZeroBeforeSet(t *testing.T) {
	t.Parallel()
	_, reader := newGauges(t)

	ms := collect(t, reader)

	assert.Zero(t, stuck(t, ms))
	assert.Equal(t, map[string]int64{
		"succeeded_no_capture": 0, "capture_not_succeeded": 0, "refund_over_capture": 0,
	}, driftByKind(t, ms))
}

// Имена и единицы — контракт: на них стоят алерты потребителя.
func TestGauges_SetIsVisibleWithContractNamesAndUnits(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)

	g.Set(paymentotel.Snapshot{
		Stuck: 7,
		Drift: append(drifts(payment.DriftSucceededNoCapture, 2), drifts(payment.DriftRefundOverCapture, 1)...),
	})

	ms := collect(t, reader)
	assert.Equal(t, int64(7), stuck(t, ms))
	assert.Equal(t, map[string]int64{
		"succeeded_no_capture": 2, "capture_not_succeeded": 0, "refund_over_capture": 1,
	}, driftByKind(t, ms))
	assert.Equal(t, unitIntent, metricByName(t, ms, stuckName).Unit)
	assert.Equal(t, unitIntent, metricByName(t, ms, driftName).Unit)

	// Последний Set вытесняет прошлый: гейдж — состояние, а не история.
	g.Set(paymentotel.Snapshot{})
	ms = collect(t, reader)
	assert.Zero(t, stuck(t, ms))
	assert.Equal(t, map[string]int64{
		"succeeded_no_capture": 0, "capture_not_succeeded": 0, "refund_over_capture": 0,
	}, driftByKind(t, ms))
}

// Род вне закрытого набора меткой не становится и не теряется: запись идёт
// рядом без kind, и алерт payment_drift > 0 горит. Ряд живёт, пока жива запись.
func TestGauges_UnknownKindIsCountedWithoutLabel(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)

	g.Set(paymentotel.Snapshot{
		Drift: append(drifts("made_up_by_store", 2), drifts(payment.DriftCaptureNotSucceeded, 1)...),
	})

	got := driftByKind(t, collect(t, reader))
	assert.Equal(t, int64(2), got[noKind], "запись с чужим родом видна")
	assert.Equal(t, int64(1), got["capture_not_succeeded"])
	assert.NotContains(t, got, "made_up_by_store", "строка стора в метку не попала")

	g.Set(paymentotel.Snapshot{})
	assert.NotContains(t, driftByKind(t, collect(t, reader)), noKind, "ряд без kind уходит вместе с записью")
}

// Set считает записи и не держит их: в записи плательщик и ссылка заказа, и
// правка среза после Set снимок не меняет.
func TestGauges_SetKeepsCountsNotRecords(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)
	records := drifts(payment.DriftSucceededNoCapture, 1)

	g.Set(paymentotel.Snapshot{Drift: records})
	records[0].Kind = payment.DriftRefundOverCapture

	got := driftByKind(t, collect(t, reader))
	assert.Equal(t, int64(1), got["succeeded_no_capture"])
	assert.Zero(t, got["refund_over_capture"])
}

// Set зовёт задание сверки, коллбэк — экспортёр на scrape: они идут
// параллельно по определению.
func TestGauges_ConcurrentSetAndCollect(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)

	const writers, iterations = 4, 200
	collectErrs := make(chan error, iterations)

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iterations {
				g.Set(paymentotel.Snapshot{
					Stuck: int64(w*iterations + i),
					Drift: drifts(payment.DriftRefundOverCapture, i%3),
				})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			var rm metricdata.ResourceMetrics
			collectErrs <- reader.Collect(context.Background(), &rm)
		}
	}()
	wg.Wait()
	close(collectErrs)

	for collectErr := range collectErrs {
		require.NoError(t, collectErr)
	}
}

// Unregister снимает ряды со scrape и безвреден при повторе: тому, кто
// пересобирает сервис на лету, иначе пришлось бы считать свои вызовы.
func TestGauges_Unregister(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)
	g.Set(paymentotel.Snapshot{Stuck: 5})
	require.Len(t, collect(t, reader), 2)

	require.NoError(t, g.Unregister())

	assert.Empty(t, collect(t, reader), "снятый коллбэк рядов не даёт")
	require.NoError(t, g.Unregister(), "идемпотентен")
	assert.NotPanics(t, func() { g.Set(paymentotel.Snapshot{Stuck: 9}) }, "Set после снятия безвреден")
}

func TestNewGauges_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { _, _ = paymentotel.NewGauges(nil) })
}

// Отказ метра возвращается ошибкой на каждом шаге сборки, а не роняет процесс.
func TestNewGauges_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	for _, failOn := range []string{stuckName, driftName, failCallback} {
		t.Run(failOn, func(t *testing.T) {
			t.Parallel()
			var (
				g   *paymentotel.Gauges
				err error
			)

			assert.NotPanics(t, func() { g, err = paymentotel.NewGauges(failingMeter{failOn: failOn}) })

			require.ErrorIs(t, err, errMeter)
			assert.Nil(t, g)
		})
	}
}
