package payment

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMoney_Bounds(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		minor    int64
		currency string
		ok       bool
	}{
		"ноль законен":      {0, "RUB", true},
		"ровно потолок":     {MaxMoneyMinor, "RUB", true},
		"на копейку выше":   {MaxMoneyMinor + 1, "RUB", false},
		"отрицательная":     {-1, "RUB", false},
		"строчная валюта":   {100, "rub", false},
		"валюта из двух":    {100, "RU", false},
		"пустая валюта":     {100, "", false},
		"валюта с пробелом": {100, "RU ", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := NewMoney(tc.minor, tc.currency)
			if !tc.ok {
				require.ErrorIs(t, err, ErrInvalidMoney)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.minor, m.Minor())
			assert.Equal(t, tc.currency, m.Currency())
		})
	}
}

// Сравнение идёт ВМЕСТЕ с валютой: провайдер, настроенный не на тот магазин,
// пришлёт ту же цифру в другой валюте, и сравнение по числу пропустило бы её.
func TestMoney_EqualComparesCurrency(t *testing.T) {
	t.Parallel()

	rub, err := NewMoney(79900, "RUB")
	require.NoError(t, err)
	kzt, err := NewMoney(79900, "KZT")
	require.NoError(t, err)

	assert.False(t, rub.Equal(kzt))
	assert.True(t, rub.Equal(mustMoney(t, 79900)))
}

func TestMoney_ArithmeticPanicsOnMixedCurrency(t *testing.T) {
	t.Parallel()

	rub := mustMoney(t, 100)
	kzt, err := NewMoney(100, "KZT")
	require.NoError(t, err)

	assert.Panics(t, func() { _ = rub.Add(kzt) })
	assert.Panics(t, func() { _ = rub.Sub(kzt) })
	assert.Panics(t, func() { _, _ = rub.tryAdd(kzt) })
	assert.Panics(t, func() { _ = ZeroMoney("рубль") })
}

// Нетто считается по книге, а не по колонке «оплачено»: материализованная сумма
// разъезжается с книгой молча, а книга расхождение показывает.
func TestNet(t *testing.T) {
	t.Parallel()

	intentID := uuid.New()
	entry := func(kind LedgerKind, minor int64, currency string) LedgerEntry {
		return LedgerEntry{
			ID: uuid.New(), IntentID: intentID, Kind: kind, AmountMinor: minor, Currency: currency,
		}
	}

	t.Run("частичные возвраты складываются", func(t *testing.T) {
		t.Parallel()

		net, err := Net([]LedgerEntry{
			entry(LedgerCapture, 100000, "RUB"),
			entry(LedgerRefund, 30000, "RUB"),
			entry(LedgerRefund, 20000, "RUB"),
		}, "RUB")

		require.NoError(t, err)
		assert.Equal(t, int64(50000), net.Minor())
	})

	t.Run("полный возврат обнуляет", func(t *testing.T) {
		t.Parallel()

		net, err := Net([]LedgerEntry{
			entry(LedgerCapture, 100000, "RUB"),
			entry(LedgerRefund, 100000, "RUB"),
		}, "RUB")

		require.NoError(t, err)
		assert.True(t, net.IsZero())
	})

	t.Run("пустая книга — ноль в валюте книги", func(t *testing.T) {
		t.Parallel()

		net, err := Net(nil, "RUB")

		require.NoError(t, err)
		assert.True(t, net.IsZero())
		assert.Equal(t, "RUB", net.Currency())
	})

	t.Run("возвратов больше зачислений — это видно, а не clamp", func(t *testing.T) {
		t.Parallel()

		net, err := Net([]LedgerEntry{
			entry(LedgerCapture, 100, "RUB"),
			entry(LedgerRefund, 300, "RUB"),
		}, "RUB")

		require.NoError(t, err)
		assert.Equal(t, int64(-200), net.Minor())
	})

	t.Run("чужая валюта в книге — отказ считать", func(t *testing.T) {
		t.Parallel()

		_, err := Net([]LedgerEntry{entry(LedgerCapture, 100, "KZT")}, "RUB")

		require.ErrorIs(t, err, ErrInvalidMoney)
	})

	t.Run("неизвестный род записи — отказ считать", func(t *testing.T) {
		t.Parallel()

		_, err := Net([]LedgerEntry{entry("bonus", 100, "RUB")}, "RUB")

		require.ErrorIs(t, err, ErrInvalidMoney)
	})

	t.Run("негодная сумма записи — отказ считать", func(t *testing.T) {
		t.Parallel()

		_, err := Net([]LedgerEntry{entry(LedgerCapture, -1, "RUB")}, "RUB")

		require.ErrorIs(t, err, ErrInvalidMoney)
	})
}
