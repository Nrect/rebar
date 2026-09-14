package mail_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом обращении к часам.
func TestService_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	assert.PanicsWithValue(t, "mail.Service.SetClock: now must not be nil", func() { svc.SetClock(nil) })
}
