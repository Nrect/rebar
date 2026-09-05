package mailotel_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailotel"
)

// До первого Set гейджи обязаны отдавать нули, а не отсутствовать: пустой
// ряд на дашборде неотличим от «экспортёр отвалился».
func TestGauges_ZeroBeforeSet(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	_, err := mailotel.NewGauges(meter)
	require.NoError(t, err)

	ms := collect(t, reader)
	assert.Equal(t, int64(0), gaugeInt64(t, ms, pendingName))
	assert.InDelta(t, 0.0, oldestAge(t, ms), 1e-9)
	assert.Equal(t, int64(0), gaugeInt64(t, ms, failedName))
}

// Имена и единицы — контракт: на них стоят алерты потребителя, а по единице s
// Prometheus-экспортёр припишет _seconds.
func TestGauges_SetIsVisibleWithContractNamesAndUnits(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	g, err := mailotel.NewGauges(meter)
	require.NoError(t, err)

	g.Set(mail.Stats{Pending: 7, OldestPendingAge: 90 * time.Second, Failed: 2})

	ms := collect(t, reader)
	assert.Equal(t, int64(7), gaugeInt64(t, ms, pendingName))
	assert.InDelta(t, 90.0, oldestAge(t, ms), 1e-9)
	assert.Equal(t, int64(2), gaugeInt64(t, ms, failedName))

	assert.Equal(t, unitEmail, metricByName(t, ms, pendingName).Unit)
	assert.Equal(t, unitSeconds, metricByName(t, ms, oldestName).Unit)
	assert.Equal(t, unitEmail, metricByName(t, ms, failedName).Unit)

	// Последний Set вытесняет прошлый: гейдж — состояние, а не история.
	g.Set(mail.Stats{})
	ms = collect(t, reader)
	assert.Equal(t, int64(0), gaugeInt64(t, ms, pendingName))
	assert.InDelta(t, 0.0, oldestAge(t, ms), 1e-9)
}

// Дробный возраст не округляется до секунд: алерт oldest_pending_age > 600
// читает именно это значение.
func TestGauges_AgeIsFractionalSeconds(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	g, err := mailotel.NewGauges(meter)
	require.NoError(t, err)

	g.Set(mail.Stats{OldestPendingAge: 1500 * time.Millisecond})

	assert.InDelta(t, 1.5, oldestAge(t, collect(t, reader)), 1e-9)
}

// Set зовёт воркер после Deliver, коллбэк — экспортёр на scrape: они идут
// параллельно по определению.
func TestGauges_ConcurrentSetAndCollect(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	g, err := mailotel.NewGauges(meter)
	require.NoError(t, err)

	const writers, iterations = 4, 200
	collectErrs := make(chan error, iterations)

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iterations {
				g.Set(mail.Stats{
					Pending:          int64(w*iterations + i),
					OldestPendingAge: time.Duration(i) * time.Second,
					Failed:           int64(i),
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
