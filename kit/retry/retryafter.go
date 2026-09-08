package retry

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfterSeconds — сколько секунд ещё влезает в time.Duration.
const maxRetryAfterSeconds = uint64(math.MaxInt64 / int64(time.Second))

// ParseRetryAfter — заголовок Retry-After: секунды или HTTP-date (RFC 9110).
// Мусор и пустая строка — (0, false): «подсказки нет». Дата в прошлом —
// (0, true): подсказка есть, ждать нечего.
func ParseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(header)
	if value == "" {
		return 0, false
	}
	if d, ok := delaySeconds(value); ok {
		return d, true
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(deadline.Sub(now), 0), true
}

// delaySeconds — форма 1*DIGIT. Знак и пробелы внутри не принимаются: это
// уже не delay-seconds, и угадывать намерение провайдера незачем.
func delaySeconds(value string) (time.Duration, bool) {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err != nil || seconds > maxRetryAfterSeconds {
		// Абсурдный срок — не мусор, а «очень долго»: потолок Duration даёт
		// ErrRetryAfterTooLong, тогда как (0, false) отправил бы клиента
		// стучаться туда же по обычной экспоненте.
		return math.MaxInt64, true
	}
	return time.Duration(seconds) * time.Second, true
}
