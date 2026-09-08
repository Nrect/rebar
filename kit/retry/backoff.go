package retry

import (
	"math/rand/v2"
	"time"
)

// Backoff — экспонента с полным джиттером: delay = random(0, min(Max, Base·2^(attempt−1))).
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay — задержка перед попыткой номер attempt (1 — первая повторная).
// Потолок никогда не схлопывается в ноль: переполнение сдвига оставляет Max.
func (b Backoff) Delay(attempt int) time.Duration {
	ceiling := b.Max
	// Сдвиг на 62 и больше переполняет int64; дальше потолок и так Max.
	if attempt >= 1 && attempt < 62 {
		if exp := b.Base << uint(attempt-1); exp > 0 && exp < ceiling {
			ceiling = exp
		}
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling))) //nolint:gosec // джиттер, не криптография
}
