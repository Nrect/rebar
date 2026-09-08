package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulertest"
)

const (
	// tick — интервал задач в тестах. Мелкий намеренно: проверяется факт
	// «следующий тик пришёл», а не точность расписания.
	tick = 5 * time.Millisecond
	// wait — потолок ожидания прогонов; щедрый, потому что на загруженной
	// машине планировщик спит вместе со всеми.
	wait = 2 * time.Second
)

func noop(context.Context) (int, error) { return 0, nil }

func newSched(t *testing.T, obs scheduler.Observer, jobs ...scheduler.Job) *scheduler.Scheduler {
	t.Helper()
	s, err := scheduler.New(obs, jobs...)
	require.NoError(t, err)
	return s
}

// started запускает планировщик и гарантирует остановку до конца теста:
// забытый Stop — это горутина, живущая дольше теста, и ловится она не
// глазами, а -race в соседнем тесте.
func started(t *testing.T, s *scheduler.Scheduler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		s.Stop()
	})
	s.Start(ctx)
	return cancel
}

// recorder — наблюдатель по умолчанию.
func recorder() *schedulertest.RecordingObserver { return schedulertest.NewRecordingObserver() }
