package retry_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/retry"
)

func TestBackoff_DelayStaysInBounds(t *testing.T) {
	t.Parallel()

	b := retry.Backoff{Base: time.Second, Max: time.Minute}
	for attempt := 1; attempt <= 200; attempt++ {
		d := b.Delay(attempt)
		assert.GreaterOrEqualf(t, d, time.Duration(0), "попытка %d: отрицательная задержка", attempt)
		assert.Lessf(t, d, b.Max, "попытка %d: задержка вышла за Max", attempt)
	}
}

// Потолок растёт вдвое за попытку и упирается в Max: проверяется границей
// сверху, потому что снизу джиттер обнуляет любую попытку.
func TestBackoff_CeilingDoublesPerAttempt(t *testing.T) {
	t.Parallel()

	b := retry.Backoff{Base: time.Second, Max: time.Minute}
	for _, tc := range []struct {
		attempt int
		ceiling time.Duration
	}{
		{attempt: 1, ceiling: time.Second},
		{attempt: 2, ceiling: 2 * time.Second},
		{attempt: 3, ceiling: 4 * time.Second},
		{attempt: 7, ceiling: time.Minute}, // 64s > Max
		{attempt: 40, ceiling: time.Minute},
	} {
		for range 100 {
			assert.Lessf(t, b.Delay(tc.attempt), tc.ceiling, "попытка %d вышла за свой потолок", tc.attempt)
		}
	}
}

// Джиттер полный: за сотню выборок задержка обязана побывать и в нижней, и в
// верхней половине диапазона. Без этого «джиттер» мог бы оказаться константой.
func TestBackoff_JitterCoversWholeRange(t *testing.T) {
	t.Parallel()

	b := retry.Backoff{Base: time.Minute, Max: time.Minute}
	var low, high bool
	for range 200 {
		if b.Delay(1) < b.Max/2 {
			low = true
			continue
		}
		high = true
	}
	assert.True(t, low, "задержка ни разу не попала в нижнюю половину")
	assert.True(t, high, "задержка ни разу не попала в верхнюю половину")
}

// Переполнение сдвига обязано оставлять потолок Max, а не ноль: иначе
// джиттер пропадает ровно на дальних попытках, где он нужнее всего
// (мутант exp >= 0 из mail).
func TestBackoff_KeepsJitterAfterShiftOverflow(t *testing.T) {
	t.Parallel()

	// 20 нулевых младших битов: Base<<49 переполняется ровно в ноль.
	b := retry.Backoff{Base: 1 << 20, Max: time.Minute}

	spread := false
	for range 40 {
		if b.Delay(50) > b.Max/2 {
			spread = true
			break
		}
	}
	assert.True(t, spread, "на 50-й попытке задержка всегда мала: потолок схлопнулся в ноль")
}

func TestBackoff_ZeroValueHasNoDelay(t *testing.T) {
	t.Parallel()

	assert.Zero(t, retry.Backoff{}.Delay(1))
	assert.Zero(t, retry.Backoff{Base: time.Second}.Delay(1), "Max == 0 — потолка нет, задержки тоже")
}
