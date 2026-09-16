package inboxtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
)

// Обработчик зовётся БЕЗ замка двойника (решение 14): иначе вызов с тем же
// ключом ждал бы вместо in_flight. Закреплено без таймаутов — замок изнутри
// обработчика обязан взяться.
func TestMemStore_HandlerRunsWithoutTheLock(t *testing.T) {
	t.Parallel()

	var store *MemStore
	var lockFree bool
	store = NewMemStore(map[inbox.SourceName]Handler{
		"billing": HandlerFunc(func(context.Context, inbox.Event) error {
			lockFree = store.mu.TryLock()
			if lockFree {
				store.mu.Unlock()
			}
			return nil
		}),
	})
	outcome, err := store.Accept(t.Context(), inbox.Event{
		Source: "billing", ID: "evt-lock", Type: "invoice.paid", OccurredAt: suiteNow,
		Digest: make([]byte, inbox.DigestSize),
	}, suiteNow)
	require.NoError(t, err)
	require.Equal(t, inbox.OutcomeAccepted, outcome)
	assert.True(t, lockFree, "обработчик зовётся под замком двойника")
}
