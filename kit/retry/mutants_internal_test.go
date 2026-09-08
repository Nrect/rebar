package retry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны
// не «на функцию», а на конкретную границу, которую сдвигал мутант.
//
// Признаны эквивалентными, тестом не убиваются:
//
//   - backoff.go:19 `attempt < 62` → `<= 62`: на 62-й попытке сдвиг даёт
//     Base<<61, то есть (Base mod 8)<<61 — либо 0, либо не меньше 2^61 нс
//     (73 года). Оба значения отсекают guard'ы exp > 0 && exp < ceiling.
//   - backoff.go:20 `exp < ceiling` → `<= ceiling`: при равенстве
//     присваивается то же значение, что уже лежит в ceiling.
//   - retry.go:113 `hint > d` → `hint >= d`: при равенстве обе ветки дают
//     одну и ту же задержку.
//   - retryafter.go:12 `math.MaxInt64 / int64(time.Second)`: объявление
//     константы, покрытие его не видит; мутант с `*` переполняет константное
//     выражение и не компилируется.
//
// Мутант retry.go:87 `attempt++` → `attempt--` уходит в вечный цикл и ловится
// таймаутом, а не тестом: это обнаружение, а не выживший.

// Сон по умолчанию обязан именно ждать: мутант, который возвращает управление
// сразу, превращает экспоненту в busy-retry и выносит провайдера.
func TestSleep_ActuallyWaits(t *testing.T) {
	t.Parallel()

	start := time.Now()
	require.NoError(t, sleep(t.Context(), 20*time.Millisecond))
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
}

// Нулевая задержка не глотает отмену: следующая попытка не должна уходить в
// работу с мёртвым контекстом.
func TestSleep_ZeroDelayReportsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for range 20 {
		require.ErrorIs(t, sleep(ctx, 0), context.Canceled)
	}
	assert.NoError(t, sleep(t.Context(), 0), "живой контекст и нулевая задержка — не ошибка")
}

// Отрицательная задержка (часы ушли назад у вызывающего) — не сон в прошлое.
func TestSleep_NegativeDelayDoesNotWait(t *testing.T) {
	t.Parallel()

	start := time.Now()
	require.NoError(t, sleep(t.Context(), -time.Hour))
	assert.Less(t, time.Since(start), time.Second)
}
