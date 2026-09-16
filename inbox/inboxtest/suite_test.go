package inboxtest_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
)

var (
	currentSecret  = []byte("inboxtest-current-secret-0123456789")
	previousSecret = []byte("inboxtest-previous-secret-987654321")
)

// Двойник проходит тот же набор, что обязан пройти адаптер.
func TestMemStore_Suite(t *testing.T) {
	t.Parallel()

	inboxtest.RunStoreSuite(t, func(_ *testing.T, handlers map[inbox.SourceName]inboxtest.Handler) (inbox.Store, inboxtest.Reader) {
		store := inboxtest.NewMemStore(handlers)
		return store, store
	})
}

// Тестовый верификатор проходит набор для верификаторов проекта: набор не
// требует того, чего не умеет честная схема с подписью по байтам и временем.
func TestHMACVerifier_Suite(t *testing.T) {
	t.Parallel()

	inboxtest.RunVerifierSuite(t, func(_ *testing.T, now func() time.Time) inboxtest.VerifierFixture {
		body := func(n int) []byte {
			return inboxtest.EventBody(fmt.Sprintf("evt_%d", n), "invoice.paid", map[string]int{"invoice": n})
		}
		return inboxtest.VerifierFixture{
			Source:    "billing",
			Verifier:  inboxtest.NewHMACVerifier("billing", 5*time.Minute, now, currentSecret, previousSecret),
			Sign:      func(n int, at time.Time) inbox.Request { return inboxtest.SignHMAC(currentSecret, at, body(n)) },
			SignOther: func(n int, at time.Time) inbox.Request { return inboxtest.SignHMAC(previousSecret, at, body(n)) },
			Tolerance: 5 * time.Minute,
		}
	})
}
