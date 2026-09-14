package authzpg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/authz/authzpg"
)

// nil-часы падают на настройке, а не разыменованием nil на первой проверке прав.
func TestStore_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "authzpg.SetClock: now must not be nil", func() { new(authzpg.Store).SetClock(nil) })
}
