package idemtest_test

import (
	"testing"
	"time"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemtest"
)

// Двойник проходит тот же набор, что обязан пройти idempg.
func TestMemStore_Suite(t *testing.T) {
	t.Parallel()

	idemtest.RunDoSuite(t, func(_ *testing.T, cfg idem.Config, obs idem.Observer, now func() time.Time) idemtest.Subject {
		store := idemtest.NewMemStore(cfg, obs)
		store.SetClock(now)
		return idemtest.Subject{Do: store.Do, Pruner: store}
	})
}
