package payment_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом обращении к часам.
func TestService_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	assert.PanicsWithValue(t, "payment.Service.SetClock: now must not be nil", func() { h.svc.SetClock(nil) })
}
