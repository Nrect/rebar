package inboxtest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
)

// suiteRace — N доставок одного события разом: обработчик первой держит ключ,
// пока остальные не ответят, поэтому исход детерминирован — один accepted,
// остальным in_flight, эффект ровно один. Хранилище, которое ждёт на ключе,
// упирается в suiteWait и роняет сценарий, а не вешает набор.
func suiteRace(t *testing.T, f storeFixture) {
	t.Helper()
	const deliveries = 16
	ev := suiteEvent(suiteSource, "evt-race", "race")

	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	var entered atomic.Int32
	f.rec.set(func(context.Context, inbox.Event) error {
		if entered.Add(1) > 1 {
			open() // второй вход — уже нарушение; ждать его некому
			return nil
		}
		select {
		case <-release:
		case <-time.After(suiteWait):
		}
		return nil
	})

	outcomes := make(chan inbox.Outcome, deliveries)
	var wg sync.WaitGroup
	for range deliveries {
		wg.Go(func() {
			outcome, err := f.store.Accept(context.Background(), ev, suiteNow)
			if err != nil {
				t.Errorf("параллельная доставка: неожиданная ошибка: %v", err)
			}
			outcomes <- outcome
		})
	}
	counts := collect(t, outcomes, deliveries-1, open)
	wg.Wait()
	close(outcomes)
	for outcome := range outcomes {
		counts[outcome]++
	}

	equal(t, counts[inbox.OutcomeAccepted], 1, "принятых из параллельных доставок")
	equal(t, counts[inbox.OutcomeInFlight], deliveries-1, "in_flight из параллельных доставок")
	equal(t, f.rec.count(suiteSource, ev.ID), 1, "вызовов обработчика на параллельных доставках")
	equal(t, f.accept(t, ev, suiteNow), inbox.OutcomeDuplicate, "доставка после гонки")
}

// collect — n исходов, пока первая доставка держит ключ; затем ключ отпускается.
func collect(t *testing.T, outcomes <-chan inbox.Outcome, n int, open func()) map[inbox.Outcome]int {
	t.Helper()
	defer open()
	counts := map[inbox.Outcome]int{}
	deadline := time.After(suiteWait)
	for range n {
		select {
		case outcome := <-outcomes:
			counts[outcome]++
		case <-deadline:
			t.Errorf("параллельные доставки ждали ключ дольше %s вместо in_flight", suiteWait)
			return counts
		}
	}
	return counts
}
