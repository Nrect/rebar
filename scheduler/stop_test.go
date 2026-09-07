package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/scheduler"
)

// Stop ДОЖИДАЕТСЯ текущего прогона: процесс, вышедший из-под наполовину
// применённого исхода, оставляет открытую транзакцию базе на разбор.
func TestStop_WaitsForRunningJob(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	inFlight := make(chan struct{}, 1)
	finished := make(chan struct{})

	s := newSched(t, recorder(), scheduler.Job{
		Name: "slow", Interval: tick,
		Run: func(context.Context) (int, error) {
			select {
			case inFlight <- struct{}{}:
			default:
			}
			<-release
			close(finished)
			return 0, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	<-inFlight // прогон идёт прямо сейчас
	cancel()   // отмена контекста прогон НЕ прерывает

	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("Stop вернулся, не дождавшись прогона")
	case <-time.After(20 * tick):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(wait):
		t.Fatal("Stop не дождался завершения прогона")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Stop вернулся раньше, чем задача дописала своё")
	}
}

// Повторный Stop не паникует на закрытом канале: остановка приходит и из
// cleanup сервера, и из defer вызывающего.
func TestStop_IsIdempotent(t *testing.T) {
	t.Parallel()

	s := newSched(t, recorder(), scheduler.Job{Name: "mail", Interval: time.Hour, Run: noop})
	s.Start(context.Background())

	s.Stop()
	assert.NotPanics(t, s.Stop)
}

// Stop без Start безвреден: у потребителя выход из main общий на все ветки,
// включая ту, где старт не состоялся.
func TestStop_BeforeStart(t *testing.T) {
	t.Parallel()

	s := newSched(t, recorder(), scheduler.Job{Name: "mail", Interval: time.Hour, Run: noop})
	assert.NotPanics(t, s.Stop)
}

// Повторный Start — паника: второй набор тикеров на те же задачи означал бы
// перекрывающиеся прогоны, которых пакет не допускает по построению.
func TestStart_PanicsOnSecondCall(t *testing.T) {
	t.Parallel()

	obs := recorder()
	s := newSched(t, obs, scheduler.Job{Name: "mail", Interval: time.Hour, Run: noop})
	started(t, s)

	assert.Panics(t, func() { s.Start(context.Background()) })
	assert.Len(t, obs.Starts(), 1, "несостоявшийся старт наблюдателю не показывают")
}
