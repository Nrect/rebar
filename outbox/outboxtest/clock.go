package outboxtest

import (
	"sync"
	"time"
)

// Clock — управляемые часы для Producer.SetClock и Worker.SetClock. Под
// замком, потому что в тестах на гонку их читают несколько горутин Drain.
type Clock struct {
	mu sync.Mutex
	at time.Time
}

// NewClock — часы, стоящие на at (приводится к UTC: ядро пишет времена в UTC).
func NewClock(at time.Time) *Clock {
	return &Clock{at: at.UTC()}
}

// Now — текущее показание.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Advance двигает часы вперёд.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// Set ставит часы на момент.
func (c *Clock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at.UTC()
}
