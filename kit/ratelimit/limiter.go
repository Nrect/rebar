package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrEmptyKey — ключ не вычислен. Отдельная ошибка, потому что это дефект
// вызывающего, а не превышение лимита.
var ErrEmptyKey = errors.New("ratelimit: key must not be empty")

// Limiter — token bucket на ключ в памяти процесса. Потокобезопасен.
type Limiter struct {
	cfg      Config
	interval time.Duration
	burst    int

	mu        sync.Mutex
	buckets   map[string]*bucket
	overflows int
	now       func() time.Time
}

var _ Gate = (*Limiter)(nil)

// bucket — корзина одного ключа.
type bucket struct {
	tokens int
	// filled — момент, до которого пополнение уже начислено. Отдельно от
	// seen: отказ обновляет только seen, иначе корзина копила бы токены за
	// время, которого не прошло.
	filled time.Time
	seen   time.Time
}

// New — лимитер по политике; паникует на негодной, чтобы «лимит не настроен»
// падало на старте, а не пропускало всё подряд.
func New(cfg Config) *Limiter {
	if err := cfg.validate(); err != nil {
		panic("ratelimit.New: " + err.Error())
	}
	return &Limiter{
		cfg:      cfg,
		interval: cfg.Window / time.Duration(cfg.Limit),
		burst:    cfg.burst(),
		buckets:  make(map[string]*bucket),
		now:      time.Now,
	}
}

// Allow — потратить токен ключа. Контекст не используется: обращений вовне
// нет, он в сигнатуре ради порта Gate.
func (l *Limiter) Allow(_ context.Context, key string) (Decision, error) {
	if key == "" {
		return Decision{}, ErrEmptyKey
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if b, ok = l.admit(key, now); !ok {
			// Потолок ключей выбран: сколько ждать, неизвестно, поэтому
			// честный ответ — окно целиком.
			return Decision{RetryAfter: l.cfg.Window}, nil
		}
	}

	b.seen = now
	l.refill(b, now)
	if b.tokens <= 0 {
		return Decision{RetryAfter: l.retryAfter(b, now)}, nil
	}
	b.tokens--
	return Decision{Allowed: true, Remaining: b.tokens}, nil
}

// Sweep — удалить ключи, простаивающие дольше IdleTTL. Сигнатура совпадает с
// задачей планировщика (Run(ctx) (int, error)); фоновой горутины у лимитера
// нет нарочно (doc.go, п. 4).
func (l *Limiter) Sweep(_ context.Context) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweep(l.now()), nil
}

// Stats — снимок для гейджей потребителя.
func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{Keys: len(l.buckets), Overflows: l.overflows}
}

// SetClock — подмена часов; только для тестов, зовётся до первого Allow.
func (l *Limiter) SetClock(now func() time.Time) {
	if now == nil {
		panic("ratelimit.SetClock: now must not be nil")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// admit — место для нового ключа: сначала вымести простаивающие, и только
// потом отказать. Иначе ротация ключей заняла бы память до OOM.
func (l *Limiter) admit(key string, now time.Time) (*bucket, bool) {
	if len(l.buckets) >= l.cfg.MaxKeys {
		l.sweep(now)
	}
	if len(l.buckets) >= l.cfg.MaxKeys {
		l.overflows++
		return nil, false
	}
	b := &bucket{tokens: l.burst, filled: now, seen: now}
	l.buckets[key] = b
	return b, true
}

// refill — начислить токены за прошедшее время. Целочисленно: остаток
// времени остаётся в filled и не теряется между вызовами. Отдельной ветки
// «корзина полна» нет: у полной room == 0, и она уходит в ту же ветку
// потолка.
func (l *Limiter) refill(b *bucket, now time.Time) {
	elapsed := now.Sub(b.filled)
	if elapsed < l.interval {
		return
	}
	steps := int64(elapsed / l.interval)
	if room := int64(l.burst - b.tokens); steps >= room {
		b.tokens = l.burst
		b.filled = now
		return
	}
	b.tokens += int(steps)
	b.filled = b.filled.Add(time.Duration(steps) * l.interval)
}

// retryAfter — сколько осталось до следующего токена.
func (l *Limiter) retryAfter(b *bucket, now time.Time) time.Duration {
	return max(b.filled.Add(l.interval).Sub(now), 0)
}

// sweep — удаление простаивающих; зовётся под уже взятым замком.
func (l *Limiter) sweep(now time.Time) int {
	removed := 0
	for key, b := range l.buckets {
		if now.Sub(b.seen) > l.cfg.IdleTTL {
			delete(l.buckets, key)
			removed++
		}
	}
	return removed
}
