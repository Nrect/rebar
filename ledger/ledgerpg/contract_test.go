package ledgerpg_test

import (
	"testing"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Контрактный набор ledger.Store — по двойнику и по адаптеру в одном бинаре:
// расхождение реализаций видно сразу, а не после того, как потребитель напишет
// тесты на двойнике и выкатит прод на адаптере.
//
// КАЖДЫЙ Post — СВОЯ ТРАНЗАКЦИЯ ИЗ ПУЛА: адаптер собран через New, а не WithTx,
// иначе гонки набора (условия выпуска 5 и 6) шли бы в одной транзакции и гонками
// не были бы.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	t.Run("двойник", func(t *testing.T) {
		t.Parallel()
		ledgertest.RunStoreSuite(t, func(_ *testing.T, book ledger.Book) ledger.Store {
			return ledgertest.NewMemStore(book)
		})
	})

	t.Run("адаптер", func(t *testing.T) {
		t.Parallel()
		ledgertest.RunStoreSuite(t, func(t *testing.T, book ledger.Book) ledger.Store {
			t.Helper()
			store, _ := newStore(t, book)
			return store
		})
	})
}
