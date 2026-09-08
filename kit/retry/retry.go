package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAttemptsExhausted — попытки кончились; последняя причина обёрнута %w.
	ErrAttemptsExhausted = errors.New("retry: attempts exhausted")
	// ErrRetryAfterTooLong — провайдер просит ждать дольше Policy.MaxRetryAfter.
	ErrRetryAfterTooLong = errors.New("retry: Retry-After exceeds policy limit")
)

// Sleeper — сон, прерываемый отменой ctx. Порт: в тестах подменяется на
// счётчик задержек, чтобы прогон не зависел от настоящих часов.
type Sleeper func(ctx context.Context, d time.Duration) error

// Policy — политика повторов. Нулевое значение любого поля — отказ на старте,
// а не «выключено».
type Policy struct {
	// MaxAttempts — всего попыток, включая первую.
	MaxAttempts int
	Backoff     Backoff
	// MaxRetryAfter — потолок просьбы провайдера: дольше — вернуть управление
	// планировщику, а не спать в задаче.
	MaxRetryAfter time.Duration
}

// validate — цепочка if, а не switch: мутанты в условиях case gremlins
// считает непокрытыми, и сдвиг границы прошёл бы мимо отчёта.
func (p Policy) validate() error {
	if p.MaxAttempts <= 0 {
		return errors.New("Policy.MaxAttempts must be positive")
	}
	if p.Backoff.Base <= 0 {
		return errors.New("Policy.Backoff.Base must be positive")
	}
	if p.Backoff.Max < p.Backoff.Base {
		return errors.New("Policy.Backoff.Max must be at least Policy.Backoff.Base")
	}
	if p.MaxRetryAfter <= 0 {
		return errors.New("Policy.MaxRetryAfter must be positive")
	}
	return nil
}

// Retrier — исполнитель политики. Потокобезопасен, пока не зовут SetSleeper.
type Retrier struct {
	policy  Policy
	sleeper Sleeper
}

// New — исполнитель политики; паникует на негодной, чтобы ошибка конфигурации
// падала на старте, а не на первом отказе провайдера.
func New(p Policy) *Retrier {
	if err := p.validate(); err != nil {
		panic("retry.New: " + err.Error())
	}
	return &Retrier{policy: p, sleeper: sleep}
}

// SetSleeper — подмена сна; только для тестов, зовётся до первого Do.
func (r *Retrier) SetSleeper(s Sleeper) {
	if s == nil {
		panic("retry.SetSleeper: sleeper must not be nil")
	}
	r.sleeper = s
}

// Do — повторять op, пока она не вернёт nil, не окажется постоянной или не
// кончатся попытки. Номер попытки передаётся в op: 1 — первая.
//
// op ОБЯЗАНА БЫТЬ ИДЕМПОТЕНТНОЙ: неоднозначный таймаут Do повторит, и без
// ключа идемпотентности это второй эффект у провайдера (doc.go, п. 7).
func (r *Retrier) Do(ctx context.Context, op func(ctx context.Context, attempt int) error) error {
	if op == nil {
		panic("retry.Do: op must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var last error
	for attempt := 1; attempt <= r.policy.MaxAttempts; attempt++ {
		err := op(ctx, attempt)
		if err == nil {
			return nil
		}
		last = err
		if IsPermanent(err) {
			return err
		}
		hint, throttled := RetryAfterOf(err)
		if throttled && hint > r.policy.MaxRetryAfter {
			return fmt.Errorf("%w: %w", ErrRetryAfterTooLong, err)
		}
		if attempt == r.policy.MaxAttempts {
			break
		}
		if sleepErr := r.sleeper(ctx, r.delay(attempt, hint, throttled)); sleepErr != nil {
			return fmt.Errorf("%w: %w", sleepErr, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrAttemptsExhausted, last)
}

// delay — своя экспонента, но не раньше срока, названного провайдером.
func (r *Retrier) delay(attempt int, hint time.Duration, throttled bool) time.Duration {
	d := r.policy.Backoff.Delay(attempt)
	if throttled && hint > d {
		return hint
	}
	return d
}

// sleep — сон по умолчанию: таймер против отмены ctx.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
