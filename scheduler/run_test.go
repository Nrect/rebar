package scheduler_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulertest"
)

var errRun = errors.New("прогон упал")

// ПАНИКА В ЗАДАЧЕ НЕ РОНЯЕТ ПРОЦЕСС и не срывает расписание. Без recover
// паника фоновой горутины не ловится ничем выше по стеку: упал бы весь
// сервис, включая приём вебхуков.
func TestRun_PanicIsObservedNotFatal(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "panicky", Interval: tick,
		Run: func(context.Context) (int, error) { panic("boom") },
	})
	started(t, s)

	runs := obs.Wait(t, "panicky", 2, wait)

	first := runs[0]
	assert.True(t, first.Panicked, "паника обязана быть видна флагом, а не только текстом")
	require.ErrorIs(t, first.Err, scheduler.ErrPanic)
	assert.Contains(t, first.Err.Error(), "boom", "текст паники нужен для разбора")
	assert.NotContains(t, first.Err.Error(), "goroutine", "стека в тексте ошибки нет: он уйдёт в чужой лог")
	assert.Zero(t, first.Processed, "до присваивания результата прогон не дошёл")
}

// Паника одной задачи не трогает соседнюю: горутины разные, recover свой у
// каждой. Иначе одна кривая задача выключала бы всю фоновую работу.
func TestRun_PanicDoesNotStopOtherJobs(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs,
		scheduler.Job{Name: "panicky", Interval: tick, Run: func(context.Context) (int, error) { panic("boom") }},
		scheduler.Job{Name: "healthy", Interval: tick, Run: func(context.Context) (int, error) { return 1, nil }},
	)
	started(t, s)

	runs := obs.Wait(t, "healthy", 2, wait)
	assert.False(t, runs[0].Panicked)
	assert.NoError(t, runs[0].Err)
}

// Ошибка прогона расписание не срывает: следующий тик выполняется как обычно.
// Вечно падающая задача видна по метке result, а не по мёртвому крону.
func TestRun_ErrorDoesNotStopSchedule(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "failing", Interval: tick,
		Run: func(context.Context) (int, error) { return 0, errRun },
	})
	started(t, s)

	runs := obs.Wait(t, "failing", 3, wait)
	for _, run := range runs {
		require.ErrorIs(t, run.Err, errRun)
		assert.False(t, run.Panicked, "ошибка — не паника")
	}
}

// Прогоны одной задачи не перекрываются, и тики, пришедшие во время долгого
// прогона, НЕ КОПЯТСЯ: после возврата задача не отрабатывает их пачкой.
func TestTick_SkippedWhileRunning(t *testing.T) {
	t.Parallel()

	var (
		entered  atomic.Int64
		overlaps atomic.Int64
		inRun    atomic.Bool
	)
	release := make(chan struct{})
	inFlight := make(chan struct{}, 1)

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "slow", Interval: tick,
		Run: func(context.Context) (int, error) {
			if !inRun.CompareAndSwap(false, true) {
				overlaps.Add(1)
			}
			entered.Add(1)
			select {
			case inFlight <- struct{}{}:
			default:
			}
			<-release
			inRun.Store(false)
			return 0, nil
		},
	})
	started(t, s)

	<-inFlight
	time.Sleep(20 * tick) // за это время тикер отбил бы два десятка тиков
	assert.Equal(t, int64(1), entered.Load(), "второй прогон не стартует, пока идёт первый")

	close(release)
	s.Stop()

	assert.LessOrEqual(t, entered.Load(), int64(2),
		"два десятка пропущенных тиков не превращаются в два десятка прогонов")
	assert.Zero(t, overlaps.Load(), "прогоны одной задачи не перекрываются")
}

// Прогон вне расписания: тика не ждёт, Start не требует, наблюдается как
// обычный прогон.
func TestRunNow_RunsWithoutStart(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "mail_deliver", Interval: time.Hour,
		Run: func(context.Context) (int, error) { return 7, nil },
	})

	processed, err := s.RunNow(context.Background(), "mail_deliver")
	require.NoError(t, err)
	assert.Equal(t, 7, processed)

	runs := obs.Runs("mail_deliver")
	require.Len(t, runs, 1)
	assert.Equal(t, 7, runs[0].Processed)
	assert.Empty(t, obs.Starts(), "RunNow — не Start: ряд гейджа он не засеивает")
}

// Опечатка в ручном запуске обязана быть видимой, а не тихим no-op.
func TestRunNow_UnknownJob(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{Name: "mail_deliver", Interval: time.Hour, Run: noop})

	processed, err := s.RunNow(context.Background(), "mail_purge")
	require.ErrorIs(t, err, scheduler.ErrUnknownJob)
	assert.Contains(t, err.Error(), "mail_purge", "ошибка называет то, чего не нашли")
	assert.Zero(t, processed)
	assert.Empty(t, obs.All(), "несостоявшийся прогон наблюдателю не показывают")
}

// Паника в RunNow тоже не роняет процесс: recover один на все пути прогона.
func TestRunNow_PanicIsReturnedAsError(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "panicky", Interval: time.Hour,
		Run: func(context.Context) (int, error) { panic("boom") },
	})

	processed, err := s.RunNow(context.Background(), "panicky")
	require.ErrorIs(t, err, scheduler.ErrPanic)
	assert.Zero(t, processed)
	require.Len(t, obs.Runs("panicky"), 1)
	assert.True(t, obs.Runs("panicky")[0].Panicked)
}

// Ряд гейджа последнего успеха обязан существовать С МОМЕНТА СТАРТА: без него
// алерт «крон умер» молчит именно после неудачного выката.
func TestStart_SeedsObserver(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := schedulertest.NewFakeClock(at)

	obs := recorder()
	s := newSched(t, obs,
		scheduler.Job{Name: "mail_deliver", Interval: time.Hour, Run: noop},
		scheduler.Job{Name: "mail_purge", Interval: time.Hour, Run: noop},
	)
	s.SetClock(clock.Now)
	started(t, s)

	starts := obs.Starts()
	require.Len(t, starts, 1)
	assert.Equal(t, []string{"mail_deliver", "mail_purge"}, starts[0].Jobs)
	assert.Equal(t, at, starts[0].At, "затравка — момент старта, а не ноль")
	assert.Empty(t, obs.All(), "Started зовётся ДО первого прогона")
}

// Отмена ctx останавливает тикеры всех задач; Stop после отмены корректен и
// возвращается сразу.
func TestContextCancel_StopsTickers(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "mail_deliver", Interval: tick,
		Run: func(context.Context) (int, error) { return 1, nil },
	})
	cancel := started(t, s)

	obs.Wait(t, "mail_deliver", 2, wait)
	cancel()

	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(wait):
		t.Fatal("Stop после отмены ctx обязан вернуться сразу")
	}

	after := len(obs.Runs("mail_deliver"))
	time.Sleep(20 * tick)
	assert.Len(t, obs.Runs("mail_deliver"), after, "после отмены ctx прогонов больше нет")
}

// SetClock двигает штампы прогона: Elapsed считается по подменённым часам, а
// не по настоящим — иначе тест на длительность был бы про скорость машины.
func TestSetClock_DrivesRunTimestamps(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := schedulertest.NewFakeClock(at)

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{
		Name: "mail_deliver", Interval: time.Hour,
		Run: func(context.Context) (int, error) { clock.Advance(7 * time.Second); return 0, nil },
	})
	s.SetClock(clock.Now)

	_, err := s.RunNow(context.Background(), "mail_deliver")
	require.NoError(t, err)

	runs := obs.Runs("mail_deliver")
	require.Len(t, runs, 1)
	assert.Equal(t, at, runs[0].StartedAt)
	assert.Equal(t, 7*time.Second, runs[0].Elapsed)
}

// Часы подменяются только до Start: после него их читают горутины задач, и
// подмена на ходу была бы гонкой, которую -race поймал бы у потребителя.
func TestSetClock_RejectsMisuse(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{Name: "mail_deliver", Interval: time.Hour, Run: noop})
	assert.Panics(t, func() { s.SetClock(nil) }, "nil-часы — паника, а не тихий no-op")

	started(t, s)
	assert.Panics(t, func() { s.SetClock(time.Now) })
}
