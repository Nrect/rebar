package authtest

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Генератор идентификаторов зовётся ПОД ЗАМКОМ двойника — изнутри Create, как
// default в INSERT, — и трогать сам двойник не вправе. Закреплено без таймаутов:
// пробой замка изнутри генератора обязан не пройти.
func TestMemIdentities_IDGeneratorRunsUnderTheLock(t *testing.T) {
	t.Parallel()

	m := NewMemIdentities()
	var held bool
	m.SetIDs(func() uuid.UUID {
		held = !m.mu.TryLock()
		if !held {
			m.mu.Unlock()
		}
		return uuid.New()
	})

	_, err := m.Create(t.Context(), "a@example.org", "h", time.Time{})
	require.NoError(t, err)
	assert.True(t, held, "генератор зовётся под замком двойника")
}
