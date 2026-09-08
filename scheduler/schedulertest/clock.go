package schedulertest

import (
	"sync"
	"time"
)

// FakeClock — управляемые часы для scheduler.SetClock. Потокобезопасен:
// Now зовут горутины задач, Advance — тест.
//
// ЧАСЫ НЕ УПРАВЛЯЮТ ТИКАМИ. Тикер планировщика — time.Ticker на настоящем
// времени; FakeClock двигает только штампы Run (StartedAt, Elapsed). Тест,
// который ждёт прогона, ждёт его по-настоящему — через
// RecordingObserver.Wait, а не через Advance.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock — часы, стоящие на at.
func NewFakeClock(at time.Time) *FakeClock { return &FakeClock{now: at} }

// Now — текущее время часов.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance двигает часы вперёд на d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set ставит часы на at.
func (c *FakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}
