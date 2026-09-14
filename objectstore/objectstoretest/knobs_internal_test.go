package objectstoretest

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
)

// Часы зовутся ПОД ЗАМКОМ двойника — изнутри Put, как now() в транзакции
// записи, — и трогать сам двойник не вправе. Закреплено без таймаутов: пробой
// замка изнутри часов обязан не пройти.
func TestMemStore_ClockRunsUnderTheLock(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	var held bool
	store.SetClock(func() time.Time {
		held = !store.mu.TryLock()
		if !held {
			store.mu.Unlock()
		}
		return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	})
	body := PNG(32)

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)
	assert.True(t, held, "часы зовутся под замком двойника")
}
