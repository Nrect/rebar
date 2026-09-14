package objectstore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первом обращении к часам.
func TestCollector_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	c, _ := newCollector(t, objectstore.CollectDryRun, objectstoretest.NewMemOwned())
	assert.PanicsWithValue(t, "objectstore.Collector.SetClock: now must not be nil", func() { c.SetClock(nil) })
}
