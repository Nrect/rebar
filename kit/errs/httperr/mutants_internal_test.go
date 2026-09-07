package httperr

import (
	"errors"
	"testing"
	"time"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Прогон из каталога
// модуля:
//
//	GOWORK=off gremlins unleash --timeout-coefficient 20 --workers 2 .
//
// ЛОВУШКА ИНСТРУМЕНТА, общая для всего модуля: gremlins не мутирует выражения
// в case у switch. Позиция мутанта приходится на строку case, а блок покрытия
// начинается со следующей, и мутант помечается NOT COVERED при стопроцентном
// покрытии строки. Поэтому levelOf и reqid.safeByte написаны цепочкой if — не
// ради стиля, а чтобы их границы вообще проверялись. Оставшиеся NOT COVERED —
// switch'и в читателях config (case err != nil) и в errstest: их ветки
// закрыты табличными тестами на точный текст ошибок.
//
// Признан эквивалентным, тестом не убивается:
//
//   - retryAfterSeconds, `delay <= 0` → `delay < 0`. Ветки расходятся только
//     при delay == 0, где обе дают ноль: ранний возврат отдаёт 0, а
//     вычисление — int(math.Ceil(0)). Заголовок в обоих случаях «0».

// fixedDelay — ошибка со структурным контрактом Retry-After.
type fixedDelay struct {
	error
	delay time.Duration
}

func (e fixedDelay) RetryAfter() (time.Duration, bool) { return e.delay, true }

// Непозитивная задержка даёт «0», а не отрицательное число: «-1» в заголовке
// клиент прочитает как мусор и повторит немедленно всей толпой.
func TestRetryAfterSeconds_NonPositiveDelayIsZero(t *testing.T) {
	t.Parallel()

	for _, delay := range []time.Duration{0, -time.Nanosecond, -time.Hour} {
		seconds, ok := retryAfterSeconds(fixedDelay{error: errors.New("busy"), delay: delay})
		if !ok || seconds != 0 {
			t.Errorf("задержка %s дала Retry-After %d (ok=%v), ожидался 0", delay, seconds, ok)
		}
	}
}

// Дробная секунда округляется вверх: 1.2 с вниз дало бы «1», и клиент
// вернулся бы до истечения окна.
func TestRetryAfterSeconds_RoundsUp(t *testing.T) {
	t.Parallel()

	for delay, want := range map[time.Duration]int{
		time.Nanosecond:         1,
		1200 * time.Millisecond: 2,
		time.Second:             1,
		90 * time.Second:        90,
	} {
		if seconds, _ := retryAfterSeconds(fixedDelay{error: errors.New("busy"), delay: delay}); seconds != want {
			t.Errorf("задержка %s дала %d, ожидалось %d", delay, seconds, want)
		}
	}
}
