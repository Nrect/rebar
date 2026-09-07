package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Ноль миллисекунд в Postgres означает «таймаута нет»: округление вниз тихо
// сняло бы защиту вместо того, чтобы сделать её очень короткой.
func TestMillis_RoundsUp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "меньше миллисекунды", d: 500 * time.Microsecond, want: "1"},
		{name: "одна наносекунда", d: time.Nanosecond, want: "1"},
		{name: "ровно миллисекунда", d: time.Millisecond, want: "1"},
		{name: "полтора", d: 1500 * time.Microsecond, want: "2"},
		{name: "три секунды", d: 3 * time.Second, want: "3000"},
		{name: "пятнадцать секунд", d: 15 * time.Second, want: "15000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, millis(tt.d))
		})
	}
}

func TestSetLocalStmt(t *testing.T) {
	t.Parallel()

	cfg := good()
	stmt := setLocalStmt(cfg)

	assert.Equal(t, "SET LOCAL lock_timeout = '3000ms'; SET LOCAL statement_timeout = '15000ms'", stmt)
}

func TestBackoff(t *testing.T) {
	t.Parallel()

	const base = 20 * time.Millisecond

	tests := []struct {
		name    string
		attempt int
		frac    float64
		want    time.Duration
	}{
		{name: "первая попытка — база", attempt: 1, frac: 1, want: base},
		{name: "вторая — вдвое", attempt: 2, frac: 1, want: 2 * base},
		{name: "четвёртая — восьмикратно", attempt: 4, frac: 1, want: 8 * base},
		{name: "пятая упирается в потолок 10×", attempt: 5, frac: 1, want: 10 * base},
		{name: "сотая тоже в потолке", attempt: 100, frac: 1, want: 10 * base},
		{name: "переполнение сдвига — потолок", attempt: 64, frac: 1, want: 10 * base},
		{name: "джиттер режет пополам", attempt: 2, frac: 0.5, want: base},
		{name: "нулевой джиттер — без сна", attempt: 3, frac: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, backoff(base, tt.attempt, tt.frac))
		})
	}
}

// Полный джиттер: сон равномерен на [0, потолок], а не «потолок минус чуть-чуть».
// Две транзакции, подравшиеся за одни строки, обязаны разойтись во времени.
func TestBackoff_FullJitterSpansWholeInterval(t *testing.T) {
	t.Parallel()

	const base = time.Second
	assert.Equal(t, time.Duration(0), backoff(base, 3, 0))
	assert.Equal(t, 2*time.Second, backoff(base, 3, 0.5))
	assert.Equal(t, 4*time.Second, backoff(base, 3, 1))
}
