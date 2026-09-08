package retry_test

import (
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/retry"
)

var now = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
		ok     bool
	}{
		{name: "секунды", header: "120", want: 2 * time.Minute, ok: true},
		{name: "ноль секунд", header: "0", ok: true},
		{name: "пробелы по краям", header: "  30\t", want: 30 * time.Second, ok: true},
		{name: "ведущие нули", header: "007", want: 7 * time.Second, ok: true},
		{name: "пусто", header: ""},
		{name: "одни пробелы", header: "   "},
		{name: "минус", header: "-5"},
		{name: "плюс", header: "+5"},
		{name: "дробь", header: "5.5"},
		{name: "мусор", header: "soon"},
		{name: "секунды с единицей", header: "30s"},
		{name: "пробел внутри", header: "3 0"},
		{
			name:   "HTTP-date в будущем",
			header: now.Add(90 * time.Second).Format(http.TimeFormat),
			want:   90 * time.Second,
			ok:     true,
		},
		{name: "HTTP-date в прошлом", header: now.Add(-time.Hour).Format(http.TimeFormat), ok: true},
		{name: "RFC850", header: now.Add(time.Minute).Format(time.RFC850), want: time.Minute, ok: true},
		{name: "asctime", header: now.Add(time.Minute).Format(time.ANSIC), want: time.Minute, ok: true},
		{name: "дата без зоны", header: "2026-09-08T12:00:00Z"},
		// Абсурдный срок — «очень долго», а не «подсказки нет»: политика
		// ответит ErrRetryAfterTooLong вместо повтора по экспоненте.
		{name: "секунд больше, чем влезает", header: "99999999999999999999", want: math.MaxInt64, ok: true},
		{name: "секунды на грани переполнения", header: "10000000000", want: math.MaxInt64, ok: true},
		{
			name:   "последние секунды, которые ещё влезают",
			header: "9223372036",
			want:   9223372036 * time.Second,
			ok:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := retry.ParseRetryAfter(tc.header, now)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Подсказка провайдера доходит до политики целиком: разбор заголовка и
// обёртка Throttled — один путь.
func TestParseRetryAfter_FeedsThrottled(t *testing.T) {
	t.Parallel()

	after, ok := retry.ParseRetryAfter("45", now)
	require.True(t, ok)

	hint, ok := retry.RetryAfterOf(retry.Throttled(errBoom, after))
	require.True(t, ok)
	assert.Equal(t, 45*time.Second, hint)
	assert.Equal(t, retry.ClassThrottled, retry.Classify(retry.Throttled(errBoom, after)))
}

// Заголовок приходит из внешнего мира: разбор не паникует ни на чём и не
// выдаёт отрицательный срок, иначе сон ушёл бы в прошлое.
func FuzzParseRetryAfter(f *testing.F) {
	for _, seed := range []string{
		"", "0", "120", "-1", "+1", "  ", "9999999999999999999999",
		"Mon, 02 Jan 2006 15:04:05 GMT", "Sun, 06 Nov 1994 08:49:37 GMT",
		"Sunday, 06-Nov-94 08:49:37 GMT", "Mon Jan  2 15:04:05 2006",
		"\x00", "3 0", "５", "1e9",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, header string) {
		d, ok := retry.ParseRetryAfter(header, now)
		if !ok {
			assert.Zero(t, d, "без подсказки срок обязан быть нулевым")
			return
		}
		assert.GreaterOrEqual(t, d, time.Duration(0), "отрицательный срок не бывает подсказкой")
	})
}
