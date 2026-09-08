package schedulertest

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nrect/rebar/scheduler"
)

// pollInterval — шаг опроса в Wait: тик задачи в тестах мельче секунды, ждать
// его секундами бессмысленно.
const pollInterval = time.Millisecond

// Start — один вызов Observer.Started. Их больше одного, если наблюдателя
// делят два планировщика.
type Start struct {
	Jobs []string
	At   time.Time
}

// RecordingObserver — scheduler.Observer, который всё запоминает.
// Потокобезопасен; нулевое значение готово к работе.
type RecordingObserver struct {
	mu     sync.Mutex
	starts []Start
	runs   []scheduler.Run
}

var _ scheduler.Observer = (*RecordingObserver)(nil)

// NewRecordingObserver — пустой наблюдатель.
func NewRecordingObserver() *RecordingObserver { return &RecordingObserver{} }

// Started запоминает вызов, копируя список задач: планировщик отдаёт свой
// срез, и держать на него ссылку значило бы читать чужую память.
func (o *RecordingObserver) Started(jobs []string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.starts = append(o.starts, Start{Jobs: slices.Clone(jobs), At: at})
}

// Finished запоминает прогон.
func (o *RecordingObserver) Finished(_ context.Context, run scheduler.Run) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runs = append(o.runs, run)
}

// Starts — копии вызовов Started в порядке поступления. Копия глубокая:
// тест, дописавший в полученный список задач, испортил бы запись двойника.
func (o *RecordingObserver) Starts() []Start {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Start, 0, len(o.starts))
	for _, start := range o.starts {
		out = append(out, Start{Jobs: slices.Clone(start.Jobs), At: start.At})
	}
	return out
}

// All — копии всех прогонов в порядке завершения.
func (o *RecordingObserver) All() []scheduler.Run {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.runs)
}

// Runs — прогоны одной задачи в порядке завершения.
func (o *RecordingObserver) Runs(job string) []scheduler.Run {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]scheduler.Run, 0, len(o.runs))
	for _, run := range o.runs {
		if run.Job == job {
			out = append(out, run)
		}
	}
	return out
}

// Wait ждёт n прогонов задачи и отдаёт их. Не дождался — t.Fatal: тест,
// молча получивший пустой срез, проверил бы не тот инвариант, ради которого
// написан.
func (o *RecordingObserver) Wait(tb testing.TB, job string, n int, timeout time.Duration) []scheduler.Run {
	tb.Helper()

	deadline := time.Now().Add(timeout)
	for {
		runs := o.Runs(job)
		if len(runs) >= n {
			return runs
		}
		if time.Now().After(deadline) {
			tb.Fatalf("задача %q отработала %d раз за %s, ожидалось %d", job, len(runs), timeout, n)
			return nil
		}
		time.Sleep(pollInterval)
	}
}
