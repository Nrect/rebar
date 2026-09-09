package fs_test

import (
	"testing"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/fs"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// ОДИН НАБОР, ОДИН БИНАРЬ, ДВЕ РЕАЛИЗАЦИИ. Двойник и адаптер обязаны вести
// себя одинаково: тесты потребителя пишутся на двойнике и зелены ровно тогда,
// когда зелен прод. Гонять наборы в разных бинарях — значит однажды заметить
// расхождение только на приёмке (CONVENTIONS §5, PATTERNS §7).
func TestStoreSuite(t *testing.T) {
	t.Parallel()

	t.Run("двойник", func(t *testing.T) {
		t.Parallel()
		objectstoretest.RunStoreSuite(t, func(*testing.T) objectstore.Store {
			return objectstoretest.NewMemStore()
		})
	})
	t.Run("адаптер fs", func(t *testing.T) {
		t.Parallel()
		objectstoretest.RunStoreSuite(t, func(t *testing.T) objectstore.Store {
			t.Helper()
			return newStore(t)
		})
	})
}

// newStore — адаптер над каталогом на прогон одного сценария.
func newStore(t *testing.T) *fs.Store {
	t.Helper()
	return fs.New(fs.Config{Root: t.TempDir(), BaseURL: "http://localhost:8080/files"})
}
