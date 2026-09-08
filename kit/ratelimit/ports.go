package ratelimit

import (
	"context"
	"time"
)

// Gate — порт лимитера: реализация в памяти процесса (Limiter) или общая для
// нескольких инстансов (адаптер redis, v0.2). В сигнатуре только примитивы и
// собственный тип — порт переживает смену реализации.
//
// Ошибка означает «решение не принято»: вызывающий обязан отказать, а не
// пропустить (doc.go, п. 2).
type Gate interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Decision — исход проверки. RetryAfter заполняется только при отказе.
type Decision struct {
	// Allowed — пропустить ли событие.
	Allowed bool
	// Remaining — сколько токенов осталось в корзине после решения.
	Remaining int
	// RetryAfter — через сколько появится следующий токен.
	RetryAfter time.Duration
}

// Stats — снимок для гейджей: читает потребитель по своему расписанию.
type Stats struct {
	// Keys — сколько ключей сейчас в памяти.
	Keys int
	// Overflows — сколько раз новому ключу отказали из-за MaxKeys.
	Overflows int
}
