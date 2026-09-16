package ledgertest_test

import (
	"testing"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Двойник проходит тот же набор, что обязан пройти ledgerpg.
func TestMemStore_Suite(t *testing.T) {
	t.Parallel()

	ledgertest.RunStoreSuite(t, func(_ *testing.T, book ledger.Book) ledger.Store {
		return ledgertest.NewMemStore(book)
	})
}
