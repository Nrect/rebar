package schedulertest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulertest"
)

var at = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// Runs отдаёт прогоны только запрошенной задачи и в порядке завершения:
// тесты потребителя читают исход по одной задаче.
func TestRecordingObserver_RunsFilterByJob(t *testing.T) {
	t.Parallel()

	obs := schedulertest.NewRecordingObserver()
	ctx := context.Background()
	obs.Finished(ctx, scheduler.Run{Job: "a", Processed: 1})
	obs.Finished(ctx, scheduler.Run{Job: "b", Processed: 2})
	obs.Finished(ctx, scheduler.Run{Job: "a", Processed: 3})

	runs := obs.Runs("a")
	require.Len(t, runs, 2)
	assert.Equal(t, 1, runs[0].Processed)
	assert.Equal(t, 3, runs[1].Processed)
	assert.Len(t, obs.All(), 3)
	assert.Empty(t, obs.Runs("c"))
}

// Двойник копирует и то, что ему дали, и то, что отдаёт: планировщик отдаёт
// свой срез имён, а тест, дописавший в полученный, испортил бы запись.
func TestRecordingObserver_CopiesSlices(t *testing.T) {
	t.Parallel()

	obs := schedulertest.NewRecordingObserver()
	jobs := []string{"a", "b"}
	obs.Started(jobs, at)
	jobs[0] = "испорчено"

	starts := obs.Starts()
	require.Len(t, starts, 1)
	assert.Equal(t, []string{"a", "b"}, starts[0].Jobs)
	assert.Equal(t, at, starts[0].At)

	starts[0].Jobs[1] = "тоже испорчено"
	assert.Equal(t, []string{"a", "b"}, obs.Starts()[0].Jobs)
}

// Wait ждёт нужное число прогонов и отдаёт их.
func TestRecordingObserver_WaitReturnsWhenEnough(t *testing.T) {
	t.Parallel()

	obs := schedulertest.NewRecordingObserver()
	go func() {
		for i := range 3 {
			time.Sleep(time.Millisecond)
			obs.Finished(context.Background(), scheduler.Run{Job: "a", Processed: i})
		}
	}()

	runs := obs.Wait(t, "a", 3, 2*time.Second)
	assert.Len(t, runs, 3)
}

// Не дождался — валит тест, а не отдаёт пустое: тест, молча получивший
// пустой срез, проверил бы не тот инвариант, ради которого написан.
func TestRecordingObserver_WaitFailsOnTimeout(t *testing.T) {
	t.Parallel()

	obs := schedulertest.NewRecordingObserver()
	fake := &fakeTB{}

	runs := obs.Wait(fake, "a", 1, 5*time.Millisecond)
	assert.Nil(t, runs)
	assert.Contains(t, fake.failed, `"a"`, "сообщение называет задачу и счёт")
	assert.Contains(t, fake.failed, "0")
}

// Наблюдателя делят горутины задач и тест: -race обязан быть чистым.
func TestRecordingObserver_IsRaceFree(t *testing.T) {
	t.Parallel()

	obs := schedulertest.NewRecordingObserver()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				obs.Started([]string{"a"}, at)
				obs.Finished(context.Background(), scheduler.Run{Job: "a", Processed: w})
				_ = obs.Runs("a")
				_ = obs.Starts()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, obs.All(), 400)
}

// fakeTB — подставной testing.TB: Wait зовёт только Helper и Fatalf, и
// провал двойника проверяется без падения настоящего теста.
type fakeTB struct {
	testing.TB
	failed string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) { f.failed = fmt.Sprintf(format, args...) }
