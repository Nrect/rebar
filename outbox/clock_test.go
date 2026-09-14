package outbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом обращении к часам.
func TestProducerAndWorker_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	assert.PanicsWithValue(t, "outbox.Producer.SetClock: now must not be nil", func() { h.prod.SetClock(nil) })
	assert.PanicsWithValue(t, "outbox.Worker.SetClock: now must not be nil", func() { h.worker.SetClock(nil) })
}
