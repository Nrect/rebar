package prorate_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment/prorate"
)

const day = 24 * time.Hour

func TestProrate_UnusedPeriod(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		total int64
		used  int
		whole int
		want  int64
	}{
		"сроком не пользовались — возвращаем всё": {100000, 0, 365, 100000},
		"использовали пять дней из 365":           {100000, 5, 365, 98631},
		"половина срока":                          {1000, 5, 10, 500},
		"срок выбран целиком":                     {100000, 365, 365, 0},
		"ноль возвращает ноль":                    {0, 5, 365, 0},
		"остаток в один день":                     {100000, 364, 365, 274},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := prorate.Prorate(tc.total, tc.used, tc.whole)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Округление вверх, в пользу покупателя: спор о копейке стоит дороже копейки.
func TestProrate_RoundsUpInBuyersFavour(t *testing.T) {
	t.Parallel()

	// 100 · 2 / 3 = 66.67 — покупателю 67, а не 66.
	got, err := prorate.Prorate(100, 1, 3)

	require.NoError(t, err)
	assert.Equal(t, int64(67), got)
}

// Итог НИКОГДА не превышает исходную сумму: вернуть больше полученного нельзя.
func TestProrate_NeverExceedsTotal(t *testing.T) {
	t.Parallel()

	for used := range 366 {
		got, err := prorate.Prorate(99999, used, 365)
		require.NoError(t, err)
		assert.LessOrEqual(t, got, int64(99999))
		assert.GreaterOrEqual(t, got, int64(0))
	}
}

func TestProrate_Rejects(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		total int64
		used  int
		whole int
		is    error
	}{
		"нулевой срок":         {100, 0, 0, prorate.ErrInvalidPeriod},
		"отрицательный срок":   {100, 0, -1, prorate.ErrInvalidPeriod},
		"срок за потолком":     {100, 0, prorate.MaxUnits + 1, prorate.ErrInvalidPeriod},
		"использовано больше":  {100, 6, 5, prorate.ErrInvalidPeriod},
		"отрицательно исполь.": {100, -1, 5, prorate.ErrInvalidPeriod},
		"отрицательная сумма":  {-1, 0, 5, prorate.ErrInvalidAmount},
		"сумма за потолком":    {prorate.MaxTotal + 1, 0, 5, prorate.ErrInvalidAmount},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := prorate.Prorate(tc.total, tc.used, tc.whole)

			require.ErrorIs(t, err, tc.is)
		})
	}
}

// Потолок ровно в MaxTotal законен: сдвиг границы внутрь запретил бы возврат по
// предельной сумме.
func TestProrate_ExactlyAtCap(t *testing.T) {
	t.Parallel()

	got, err := prorate.Prorate(prorate.MaxTotal, 0, prorate.MaxUnits)

	require.NoError(t, err)
	assert.Equal(t, prorate.MaxTotal, got)
}

// Пропорция берётся позициями: Σ позиций и есть сумма возврата, иначе чек не
// сойдётся с расчётом.
func TestSplit_SumIsTheRefundAmount(t *testing.T) {
	t.Parallel()

	parts, err := prorate.Split([]int64{699, 499}, 1, 3)

	require.NoError(t, err)
	// Каждая позиция округлена вверх по отдельности: 466 и 333.
	assert.Equal(t, []int64{466, 333}, parts)
	assert.Equal(t, int64(799), prorate.Sum(parts))
}

// Σ по позициям может быть на копейки БОЛЬШЕ, чем округлённый итог: это не
// расхождение, а цена сходимости чека.
func TestSplit_MayExceedRoundedTotal(t *testing.T) {
	t.Parallel()

	parts, err := prorate.Split([]int64{699, 499}, 1, 3)
	require.NoError(t, err)
	whole, err := prorate.Prorate(699+499, 1, 3)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, prorate.Sum(parts), whole)
}

func TestSplit_Rejects(t *testing.T) {
	t.Parallel()

	_, err := prorate.Split([]int64{100}, 0, 0)
	require.ErrorIs(t, err, prorate.ErrInvalidPeriod)

	_, err = prorate.Split([]int64{-1}, 0, 5)
	require.ErrorIs(t, err, prorate.ErrInvalidAmount)

	// Каждая позиция в потолке, а их сумма — нет: отказ, а не молчаливое
	// переполнение.
	_, err = prorate.Split([]int64{prorate.MaxTotal, prorate.MaxTotal}, 0, 5)
	require.ErrorIs(t, err, prorate.ErrInvalidAmount)
}

func TestSplit_EmptyIsEmpty(t *testing.T) {
	t.Parallel()

	parts, err := prorate.Split(nil, 1, 5)

	require.NoError(t, err)
	assert.Empty(t, parts)
	assert.Zero(t, prorate.Sum(parts))
}

func TestUsedUnits(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		to    time.Time
		whole int
		want  int
	}{
		"в тот же миг":                      {from, 365, 0},
		"неполные сутки не считаются":       {from.Add(day - time.Second), 365, 0},
		"ровно сутки":                       {from.Add(day), 365, 1},
		"сутки и час":                       {from.Add(day + time.Hour), 365, 1},
		"больше срока — зажимаем":           {from.Add(400 * day), 365, 365},
		"ровно срок":                        {from.Add(365 * day), 365, 365},
		"часы назад — ноль в пользу покуп.": {from.Add(-10 * day), 365, 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := prorate.UsedUnits(from, tc.to, day, tc.whole)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUsedUnits_Rejects(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err := prorate.UsedUnits(from, from, 0, 365)
	require.ErrorIs(t, err, prorate.ErrInvalidUnit)

	_, err = prorate.UsedUnits(from, from, time.Millisecond, 365)
	require.ErrorIs(t, err, prorate.ErrInvalidUnit)

	_, err = prorate.UsedUnits(from, from, day, 0)
	require.ErrorIs(t, err, prorate.ErrInvalidPeriod)
}

// Единица срока — ровно unit секунд, без календаря: перевод часов не должен
// менять сумму возврата.
func TestUsedUnits_IgnoresCalendar(t *testing.T) {
	t.Parallel()

	// 2026-03-29 — переход на летнее время в Европе; в UTC суток по-прежнему 24 часа.
	from := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	got, err := prorate.UsedUnits(from, from.Add(2*day), day, 10)

	require.NoError(t, err)
	assert.Equal(t, 2, got)
}

func FuzzProrate(f *testing.F) {
	f.Add(int64(100000), 5, 365)
	f.Add(int64(0), 0, 1)
	f.Add(prorate.MaxTotal, prorate.MaxUnits, prorate.MaxUnits)

	f.Fuzz(func(t *testing.T, total int64, used, whole int) {
		got, err := prorate.Prorate(total, used, whole)
		if err != nil {
			return
		}
		// Возврат не бывает отрицательным и не бывает больше полученного: и то и
		// другое — деньги из ниоткуда.
		assert.GreaterOrEqual(t, got, int64(0))
		assert.LessOrEqual(t, got, total)
		if used == whole {
			assert.Zero(t, got, "срок выбран целиком — возвращать нечего")
		}
		if used == 0 {
			assert.Equal(t, total, got, "сроком не пользовались — возвращаем всё")
		}
	})
}
