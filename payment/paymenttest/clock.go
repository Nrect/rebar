package paymenttest

import (
	"sync"
	"time"
)

// Clock — управляемые часы: payment.Service.SetClock(clock.Now).
//
// Потокобезопасны, потому что часы читает каждый запрос, а тесты гоняют
// конкурентные вебхуки под -race: голое поле time.Time там ловится гонкой, а не
// глазом.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock — часы, стоящие на t (в UTC).
func NewClock(t time.Time) *Clock { return &Clock{now: t.UTC()} }

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
	c.now = t.UTC()
}

// Advance двигает часы вперёд на d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
