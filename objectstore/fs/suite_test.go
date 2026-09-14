package fs_test

import (
	"testing"
	"time"

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
			store := objectstoretest.NewMemStore()
			// Часы в чужой зоне и с наносекундами: на часах в UTC сценарий зоны
			// у двойника зелен и без приведения к UTC.
			store.SetClock(func() time.Time { return foreignMoment })
			return store
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

// foreignMoment — момент часов двойника в наборе.
var foreignMoment = time.Date(2026, 9, 14, 12, 0, 0, 123456789, time.FixedZone("UTC+3", 3*60*60))

// newStore — адаптер над каталогом на прогон одного сценария.
func newStore(t *testing.T) *fs.Store {
	t.Helper()
	return fs.New(fs.Config{Root: t.TempDir(), BaseURL: "http://localhost:8080/files"})
}
