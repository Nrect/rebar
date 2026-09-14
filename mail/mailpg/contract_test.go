package mailpg_test

import (
	"testing"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// ОДИН НАБОР НА ДВЕ РЕАЛИЗАЦИИ, И ОБЕ — В ОДНОМ БИНАРЕ. Сценарий физически
// один (mailtest.RunStoreSuite), поэтому двойник и адаптер не могут
// разойтись незаметно. Двойник проходит его и без Docker — под -short
// пропускается только адаптер.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	t.Run("mailtest.MemStore", func(t *testing.T) {
		t.Parallel()
		mailtest.RunStoreSuite(t, func(*testing.T) mail.Store { return mailtest.NewMemStore() })
	})

	t.Run("mailpg.Store", func(t *testing.T) {
		t.Parallel()
		mailtest.RunStoreSuite(t, func(t *testing.T) mail.Store {
			t.Helper()
			store, _ := newStore(t)
			return store
		})
	})
}
