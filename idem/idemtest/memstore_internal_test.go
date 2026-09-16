package idemtest

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

// ИСКЛЮЧЕНИЕ ПО КЛЮЧУ, А НЕ ОБЩИМ ЗАМКОМ: op идёт без замка двойника, иначе
// параллельный вызов ждал бы вместо in_flight (ADR-0012, решение 14).
// Закреплено без таймаутов: замок берётся изнутри op.
func TestMemStore_OpRunsWithoutTheLock(t *testing.T) {
	t.Parallel()

	store := NewMemStore(suiteConfig(), NewObserver())
	key, err := idem.ParseKey("k")
	require.NoError(t, err)
	req := idem.Request{
		Scope: idem.Scope{Realm: suiteRealm, Subject: "s"}, Operation: opCreate, Key: key, Method: http.MethodPost,
	}
	free := false
	_, err = store.Do(t.Context(), req, func(context.Context) (idem.Response, error) {
		free = store.mu.TryLock()
		if free {
			store.mu.Unlock()
		}
		return created(1), nil
	})
	require.NoError(t, err)
	assert.True(t, free, "op зовётся под замком двойника")
	assert.True(t, store.mu.TryLock(), "после Do замок свободен")
	store.mu.Unlock()
}
