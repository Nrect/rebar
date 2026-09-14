package audit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом обращении к часам.
func TestRecorder_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	assert.PanicsWithValue(t, "audit.Recorder.SetClock: now must not be nil", func() { rec.SetClock(nil) })
}
