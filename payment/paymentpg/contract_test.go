package paymentpg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Контрактный набор payment.Store гоняется по двойнику и по адаптеру В ОДНОМ
// БИНАРЕ: расхождение реализаций обязано быть видно сразу, а не после того, как
// потребитель напишет тесты на двойнике и выкатит прод на адаптере.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	t.Run("двойник", func(t *testing.T) {
		t.Parallel()
		paymenttest.RunStoreSuite(t, func(_ *testing.T, hook *paymenttest.Hook) payment.Store {
			mem := paymenttest.NewMemStore()
			mem.OnSettled = hook.Call
			mem.OnRefunded = hook.Call
			return mem
		})
	})

	t.Run("адаптер", func(t *testing.T) {
		t.Parallel()
		paymenttest.RunStoreSuite(t, func(t *testing.T, hook *paymenttest.Hook) payment.Store {
			t.Helper()
			store, _ := newStore(t, paymentpg.Options{Settler: suiteSettler{hook: hook}})
			return store
		})
	})
}

// suiteSettler — хук набора в форме адаптера: у двойника это функции, у
// адаптера интерфейс с транзакцией, а сценариям нужно одно и то же.
type suiteSettler struct{ hook *paymenttest.Hook }

func (s suiteSettler) OnSettled(_ context.Context, _ pgx.Tx, in payment.Intent,
	e payment.LedgerEntry,
) error {
	return s.hook.Call(in, e)
}

func (s suiteSettler) OnRefunded(_ context.Context, _ pgx.Tx, in payment.Intent,
	e payment.LedgerEntry,
) error {
	return s.hook.Call(in, e)
}
