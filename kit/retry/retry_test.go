package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/retry"
)

// recorder — фейковый сон: копит запрошенные задержки и не тратит время.
type recorder struct {
	delays []time.Duration
	err    error
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.delays = append(r.delays, d)
	return r.err
}

func policy() retry.Policy {
	return retry.Policy{
		MaxAttempts:   3,
		Backoff:       retry.Backoff{Base: time.Second, Max: time.Minute},
		MaxRetryAfter: time.Minute,
	}
}

func retrier(t *testing.T, sleeper retry.Sleeper) *retry.Retrier {
	t.Helper()
	r := retry.New(policy())
	r.SetSleeper(sleeper)
	return r
}

func TestNew_PanicsOnInvalidPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		policy  retry.Policy
		message string
	}{
		{name: "нет попыток", policy: retry.Policy{}, message: "retry.New: Policy.MaxAttempts must be positive"},
		{
			name:    "нулевая база",
			policy:  retry.Policy{MaxAttempts: 1},
			message: "retry.New: Policy.Backoff.Base must be positive",
		},
		{
			name:    "потолок ниже базы",
			policy:  retry.Policy{MaxAttempts: 1, Backoff: retry.Backoff{Base: time.Minute, Max: time.Second}},
			message: "retry.New: Policy.Backoff.Max must be at least Policy.Backoff.Base",
		},
		{
			name:    "нет потолка ожидания",
			policy:  retry.Policy{MaxAttempts: 1, Backoff: retry.Backoff{Base: time.Second, Max: time.Minute}},
			message: "retry.New: Policy.MaxRetryAfter must be positive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.PanicsWithValue(t, tc.message, func() { retry.New(tc.policy) })
		})
	}
}

func TestNew_PanicsOnNilSleeperAndOp(t *testing.T) {
	t.Parallel()

	r := retry.New(policy())
	assert.PanicsWithValue(t, "retry.SetSleeper: sleeper must not be nil", func() { r.SetSleeper(nil) })
	assert.PanicsWithValue(t, "retry.Do: op must not be nil", func() { _ = r.Do(t.Context(), nil) })
}

func TestDo_SucceedsWithoutSleepingOnFirstAttempt(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	attempts := 0
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		attempts++
		assert.Equal(t, attempts, attempt, "номер попытки обязан идти с единицы подряд")
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, attempts)
	assert.Empty(t, rec.delays, "успех с первой попытки не спит")
}

func TestDo_RetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	attempts := 0
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		attempts = attempt
		if attempt < 3 {
			return errBoom
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
	assert.Len(t, rec.delays, 2, "между тремя попытками ровно два сна")
}

func TestDo_ExhaustedWrapsLastCause(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	last := errors.New("последняя причина")
	attempts := 0
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		attempts = attempt
		if attempt == 3 {
			return last
		}
		return errBoom
	})

	require.ErrorIs(t, err, retry.ErrAttemptsExhausted)
	require.ErrorIs(t, err, last, "причина обязана остаться в цепочке")
	assert.Equal(t, 3, attempts, "попыток ровно MaxAttempts")
	assert.Len(t, rec.delays, 2, "после последней попытки не спим")
	assert.False(t, retry.IsPermanent(err), "исчерпание попыток не делает ошибку постоянной")
}

func TestDo_PermanentStopsImmediately(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	attempts := 0
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, _ int) error {
		attempts++
		return retry.Permanent(errBoom)
	})

	require.ErrorIs(t, err, errBoom)
	assert.Equal(t, 1, attempts, "постоянная ошибка не повторяется")
	assert.Empty(t, rec.delays)
	assert.NotErrorIs(t, err, retry.ErrAttemptsExhausted, "постоянная ошибка возвращается как есть")
}

func TestDo_HonoursRetryAfterHint(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		if attempt == 1 {
			return retry.Throttled(errBoom, 30*time.Second)
		}
		return nil
	})

	require.NoError(t, err)
	require.Len(t, rec.delays, 1)
	assert.Equal(t, 30*time.Second, rec.delays[0], "просьба провайдера длиннее экспоненты — спим по ней")
}

func TestDo_KeepsOwnBackoffWhenHintIsShorter(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		if attempt == 1 {
			return retry.Throttled(errBoom, time.Nanosecond)
		}
		return nil
	})

	require.NoError(t, err)
	require.Len(t, rec.delays, 1)
	assert.Less(t, rec.delays[0], time.Second, "потолок первой попытки — Base")
}

func TestDo_RefusesTooLongRetryAfter(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	attempts := 0
	throttled := retry.Throttled(errBoom, time.Hour)
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, _ int) error {
		attempts++
		return throttled
	})

	require.ErrorIs(t, err, retry.ErrRetryAfterTooLong)
	require.ErrorIs(t, err, errBoom, "причина обязана остаться: по ней виден провайдер")
	after, ok := retry.RetryAfterOf(err)
	assert.True(t, ok, "срок обязан дойти до планировщика")
	assert.Equal(t, time.Hour, after)
	assert.Equal(t, 1, attempts)
	assert.Empty(t, rec.delays, "ждать час внутри задачи нельзя")
}

// Граница «слишком долго» строгая: срок ровно в MaxRetryAfter отсиживается,
// а не возвращается ошибкой — иначе политика отказывалась бы от повтора при
// значении, которое сама объявила допустимым.
func TestDo_AcceptsRetryAfterEqualToLimit(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, attempt int) error {
		if attempt == 1 {
			return retry.Throttled(errBoom, policy().MaxRetryAfter)
		}
		return nil
	})

	require.NoError(t, err)
	require.Len(t, rec.delays, 1)
	assert.Equal(t, policy().MaxRetryAfter, rec.delays[0])
}

func TestDo_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	attempts := 0
	err := retrier(t, (&recorder{}).sleep).Do(ctx, func(_ context.Context, _ int) error {
		attempts++
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, attempts, "отменённый контекст не начинает работу")
}

func TestDo_CancelInterruptsSleep(t *testing.T) {
	t.Parallel()

	rec := &recorder{err: context.Canceled}
	attempts := 0
	err := retrier(t, rec.sleep).Do(t.Context(), func(_ context.Context, _ int) error {
		attempts++
		return errBoom
	})

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errBoom, "причина последней попытки не теряется")
	assert.Equal(t, 1, attempts, "после прерванного сна попыток нет")
}

// Сон по умолчанию (без SetSleeper) обязан просыпаться на отмене, а не спать
// весь Backoff: иначе выключение сервиса ждёт минуту на каждой задаче.
func TestDo_DefaultSleeperWakesOnCancel(t *testing.T) {
	t.Parallel()

	r := retry.New(retry.Policy{
		MaxAttempts:   2,
		Backoff:       retry.Backoff{Base: time.Hour, Max: time.Hour},
		MaxRetryAfter: time.Minute,
	})
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- r.Do(ctx, func(_ context.Context, attempt int) error {
			if attempt == 1 {
				cancel()
			}
			return errBoom
		})
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("сон не прервался отменой контекста")
	}
}

// Задержка ноль не должна ни спать, ни глотать отмену: в проде это ветка
// Backoff, выдавшего ноль джиттером.
func TestDo_ZeroDelaySleepIsInstant(t *testing.T) {
	t.Parallel()

	r := retry.New(retry.Policy{
		MaxAttempts:   2,
		Backoff:       retry.Backoff{Base: time.Nanosecond, Max: time.Nanosecond},
		MaxRetryAfter: time.Minute,
	})

	start := time.Now()
	err := r.Do(t.Context(), func(_ context.Context, _ int) error { return errBoom })
	require.ErrorIs(t, err, retry.ErrAttemptsExhausted)
	assert.Less(t, time.Since(start), time.Second)
}
