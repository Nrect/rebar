package schedulerotel_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulerotel"
)

var errRun = errors.New("прогон упал")

// Имена, единицы и типы инструментов — контракт: на них стоят алерты
// потребителя, а по единице s Prometheus-экспортёр припишет _seconds.
func TestObserver_InstrumentContract(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Started([]string{"mail_deliver"}, startedAt)
	obs.Finished(context.Background(), run("mail_deliver", 3, nil, false))

	ms := collect(t, reader)
	assert.Equal(t, unitRun, metricByName(t, ms, runsName).Unit)
	assert.Equal(t, unitSeconds, metricByName(t, ms, durationName).Unit)
	assert.Equal(t, unitItem, metricByName(t, ms, processedName).Unit)
	assert.Equal(t, unitSeconds, metricByName(t, ms, lastSuccessName).Unit)

	hist, ok := metricByName(t, ms, durationName).Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s обязан быть Float64Histogram", durationName)
	require.Len(t, hist.DataPoints, 1)
	assert.InDelta(t, 1.5, hist.DataPoints[0].Sum, 1e-9, "длительность в секундах, с дробной частью")

	assert.Equal(t, int64(1), runsFor(t, ms, "mail_deliver", schedulerotel.ResultOK))
	items, found := processedFor(t, ms, "mail_deliver")
	assert.True(t, found)
	assert.Equal(t, int64(3), items)
}

// РЯД ГЕЙДЖА СУЩЕСТВУЕТ С МОМЕНТА СТАРТА, до первого прогона: «ряда нет» для
// Prometheus — NoData, а не срабатывание, и алерт «крон умер» промолчал бы
// ровно после выката, который сломал задачу.
func TestObserver_SeedsLastSuccessOnStarted(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)

	_, found := lastSuccessFor(t, collect(t, reader), "mail_deliver")
	require.False(t, found, "до Started планировщик прогонов не обещал")

	obs.Started([]string{"mail_deliver", "mail_purge"}, startedAt)

	for _, job := range []string{"mail_deliver", "mail_purge"} {
		at, ok := lastSuccessFor(t, collect(t, reader), job)
		require.Truef(t, ok, "ряд задачи %q обязан существовать до первого успеха", job)
		assert.InDelta(t, unixSeconds(startedAt), at, 1e-6, "затравка — момент старта, а не ноль")
	}
}

// Успех двигает гейдж вперёд, отказ и паника — нет: иначе вечно падающая
// задача выглядела бы живой.
func TestObserver_OnlySuccessAdvancesGauge(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Started([]string{"mail_deliver"}, startedAt)

	success := run("mail_deliver", 1, nil, false)
	success.StartedAt = startedAt.Add(time.Minute)
	obs.Finished(context.Background(), success)

	at, ok := lastSuccessFor(t, collect(t, reader), "mail_deliver")
	require.True(t, ok)
	assert.InDelta(t, unixSeconds(startedAt.Add(time.Minute)), at, 1e-6)

	failed := run("mail_deliver", 0, errRun, false)
	failed.StartedAt = startedAt.Add(time.Hour)
	obs.Finished(context.Background(), failed)

	panicked := run("mail_deliver", 0, scheduler.ErrPanic, true)
	panicked.StartedAt = startedAt.Add(2 * time.Hour)
	obs.Finished(context.Background(), panicked)

	after, ok := lastSuccessFor(t, collect(t, reader), "mail_deliver")
	require.True(t, ok)
	assert.InDelta(t, unixSeconds(startedAt.Add(time.Minute)), after,
		1e-6, "провалившийся прогон успехом не считается")
}

// Метка result различает три исхода: паника — баг в коде, ошибка — отказ
// внешней системы, и алерты на них разные.
func TestObserver_ResultByOutcome(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	ctx := context.Background()
	obs.Finished(ctx, run("mail_deliver", 1, nil, false))
	obs.Finished(ctx, run("mail_deliver", 0, errRun, false))
	obs.Finished(ctx, run("mail_deliver", 0, errRun, false))
	obs.Finished(ctx, run("mail_deliver", 0, scheduler.ErrPanic, true))

	ms := collect(t, reader)
	assert.Equal(t, int64(1), runsFor(t, ms, "mail_deliver", schedulerotel.ResultOK))
	assert.Equal(t, int64(2), runsFor(t, ms, "mail_deliver", schedulerotel.ResultError))
	assert.Equal(t, int64(1), runsFor(t, ms, "mail_deliver", schedulerotel.ResultPanic))
}

// Паника проверяется раньше ошибки: при панике заполнено и то и другое, и
// сведи их — метка "panic" не появилась бы никогда.
func TestObserver_PanicWinsOverError(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Finished(context.Background(), run("mail_deliver", 0, errRun, true))

	ms := collect(t, reader)
	assert.Equal(t, int64(1), runsFor(t, ms, "mail_deliver", schedulerotel.ResultPanic))
	assert.Zero(t, runsFor(t, ms, "mail_deliver", schedulerotel.ResultError))
}

// Ноль обработанных ряд СОЗДАЁТ (rate() на несуществующем ряду — NoData), а
// отрицательное число от кривой задачи в монотонный счётчик не попадает.
func TestObserver_ProcessedZeroCountsNegativeDoesNot(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Finished(context.Background(), run("empty", 0, nil, false))
	obs.Finished(context.Background(), run("broken", -5, nil, false))

	ms := collect(t, reader)
	items, found := processedFor(t, ms, "empty")
	assert.True(t, found, "ряд обязан существовать до первого обработанного элемента")
	assert.Zero(t, items)

	_, found = processedFor(t, ms, "broken")
	assert.False(t, found, "отрицательное processed счётчик не принимает")

	// Сам прогон при этом посчитан: кривой счёт элементов — не отказ прогона.
	assert.Equal(t, int64(1), runsFor(t, ms, "empty", schedulerotel.ResultOK))
	assert.Equal(t, int64(1), runsFor(t, ms, "broken", schedulerotel.ResultOK))
}

// Повторный Started (второй планировщик с той же задачей) НЕ откатывает
// метку назад: иначе чужой старт скрыл бы, что успеха давно не было.
func TestObserver_StartedDoesNotOverwriteSuccess(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	success := run("shared", 1, nil, false)
	success.StartedAt = startedAt.Add(time.Hour)
	obs.Finished(context.Background(), success)

	obs.Started([]string{"shared"}, startedAt)

	at, ok := lastSuccessFor(t, collect(t, reader), "shared")
	require.True(t, ok)
	assert.InDelta(t, unixSeconds(startedAt.Add(time.Hour)), at, 1e-6)
}

// Нулевое время в ряд не попадает: UnixNano() нулевого time.Time — огромное
// отрицательное число, и алерт увидел бы «последний успех в 1754 году».
func TestObserver_ZeroTimeIsNotObserved(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Started([]string{"mail_deliver"}, time.Time{})
	obs.Finished(context.Background(), scheduler.Run{Job: "mail_purge"})

	ms := collect(t, reader)
	for _, job := range []string{"mail_deliver", "mail_purge"} {
		_, found := lastSuccessFor(t, ms, job)
		assert.Falsef(t, found, "ряд задачи %q с нулевым временем врал бы алерту", job)
	}
}

// Набор result закрыт: значения — метки, по которым у потребителя стоят
// алерты, и смена строки ломающая (VERSIONING).
func TestAllResultsIsComplete(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]schedulerotel.Result{schedulerotel.ResultOK, schedulerotel.ResultError, schedulerotel.ResultPanic},
		schedulerotel.AllResults)
	assert.Equal(t, "ok", string(schedulerotel.ResultOK))
	assert.Equal(t, "error", string(schedulerotel.ResultError))
	assert.Equal(t, "panic", string(schedulerotel.ResultPanic))
}

// Nil-метр — паника в конструкторе, как scheduler.New на nil-наблюдателе:
// ошибка сборки обязана падать на старте.
func TestNewObserver_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { _, _ = schedulerotel.NewObserver(nil) })

	_, meter := newMeter(t)
	assert.NotPanics(t, func() {
		obs, err := schedulerotel.NewObserver(meter)
		require.NoError(t, err)
		assert.NotNil(t, obs)
	})
}

// Finished зовут горутины задач, коллбэк гейджа — экспортёр на scrape: они
// идут параллельно по определению.
func TestObserver_ConcurrentFinishAndCollect(t *testing.T) {
	t.Parallel()

	reader, obs := newObserver(t)
	obs.Started([]string{"a", "b"}, startedAt)

	const workers, iterations = 4, 200
	collectErrs := make(chan error, iterations)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job := "a"
			if w%2 == 0 {
				job = "b"
			}
			for i := range iterations {
				obs.Finished(context.Background(), run(job, i, nil, false))
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

	for err := range collectErrs {
		require.NoError(t, err)
	}
}
