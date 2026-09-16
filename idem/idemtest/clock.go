package idemtest

import (
	"sync"
	"time"
)

// Clock — управляемые часы: MemStore.SetClock(clock.Now) и
// idem.Purger.SetClock(clock.Now). Под замком: часы читает каждый
// параллельный запрос, и голое поле краснеет под -race.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock — часы, стоящие на t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now — текущее показание.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set переводит часы на t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance двигает часы вперёд на d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
