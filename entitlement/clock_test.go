package entitlement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом решении.
func TestService_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	assert.PanicsWithValue(t, "entitlement.SetClock: now must not be nil", func() { svc.SetClock(nil) })
}
