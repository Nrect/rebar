package scheduler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/scheduler"
)

// Два планировщика в одном процессе с ОДИНАКОВЫМИ именами задач и общим
// наблюдателем: ядро этого не запрещает (дубль отвергается только внутри
// одного набора), а под -race это единственный способ доказать, что общий
// Observer и общие часы не разъезжаются.
func TestRace_TwoSchedulersShareObserver(t *testing.T) {
	t.Parallel()

	obs := recorder()
	job := func(name string) scheduler.Job {
		return scheduler.Job{
			Name: name, Interval: tick,
			Run: func(context.Context) (int, error) { return 1, nil },
		}
	}

	first := newSched(t, obs, job("shared"), job("first_only"))
	second := newSched(t, obs, job("shared"), job("second_only"))

	started(t, first)
	started(t, second)

	// Прогон каждой задачи хотя бы дважды: тик проходит у обоих планировщиков.
	obs.Wait(t, "first_only", 2, wait)
	obs.Wait(t, "second_only", 2, wait)
	obs.Wait(t, "shared", 4, wait)

	// RunNow параллельно тикающим горутинам — та же общая запись наблюдателя.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			_, _ = first.RunNow(context.Background(), "shared")
		}
	}()
	for range 20 {
		_, _ = second.RunNow(context.Background(), "shared")
	}
	<-done

	assert.Len(t, obs.Starts(), 2, "каждый планировщик засеял ряд гейджа сам")
	assert.GreaterOrEqual(t, len(obs.Runs("shared")), 44)
}
