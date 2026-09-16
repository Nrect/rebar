package ledgertest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
)

// ЗАМОК ДВОЙНИКА И ЕСТЬ ТРАНЗАКЦИЯ: fn идёт под ним целиком, иначе
// параллельный постинг вклинился бы между пробой ключа и вставкой, и двойник
// стал бы мягче базы. Закреплено без таймаутов: взять замок изнутри fn нельзя.
func TestMemStore_PostRunsUnderTheLock(t *testing.T) {
	t.Parallel()

	store := NewMemStore(suiteBook())
	held := false
	require.NoError(t, store.Post(t.Context(), suiteBookName, uuid.New(), func(ledger.AccountTx, ledger.Account) error {
		held = !store.mu.TryLock()
		if !held {
			store.mu.Unlock()
		}
		return nil
	}))
	assert.True(t, held, "fn зовётся под замком двойника")
	assert.True(t, store.mu.TryLock(), "после Post замок свободен")
	store.mu.Unlock()
}
