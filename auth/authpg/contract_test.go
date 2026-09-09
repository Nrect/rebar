package authpg_test

import (
	"testing"

	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
)

// ОДИН НАБОР НА ДВЕ РЕАЛИЗАЦИИ, И ОБЕ — В ОДНОМ БИНАРЕ. Сценарий физически
// один (authtest.RunSessionsSuite), поэтому двойник и адаптер не могут
// разойтись незаметно: правка набора мгновенно видна обоим. Двойник проходит
// его и без Docker — под -short пропускается только адаптер.
func TestSessionsContract(t *testing.T) {
	t.Parallel()

	t.Run("authtest.MemSessions", func(t *testing.T) {
		t.Parallel()
		authtest.RunSessionsSuite(t, func(*testing.T) session.Sessions { return authtest.NewMemSessions() })
	})

	t.Run("authpg.Store", func(t *testing.T) {
		t.Parallel()
		authtest.RunSessionsSuite(t, func(t *testing.T) session.Sessions {
			t.Helper()
			store, _ := newStore(t)
			return store
		})
	})
}

func TestAttemptsContract(t *testing.T) {
	t.Parallel()

	t.Run("authtest.MemAttempts", func(t *testing.T) {
		t.Parallel()
		authtest.RunAttemptsSuite(t, func(*testing.T) session.Attempts { return authtest.NewMemAttempts() })
	})

	t.Run("authpg.Store", func(t *testing.T) {
		t.Parallel()
		authtest.RunAttemptsSuite(t, func(t *testing.T) session.Attempts {
			t.Helper()
			store, _ := newStore(t)
			return store
		})
	})
}
