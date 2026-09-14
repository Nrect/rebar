package s3_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/objectstore/s3"
)

// nil-часы падают на настройке, а не разыменованием nil в чужом стеке при
// первой подписи.
func TestStore_SetClockPanicsOnNil(t *testing.T) {
	t.Parallel()

	store := s3.New(s3.Config{
		Endpoint: "https://storage.example.com", Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret,
	})
	assert.PanicsWithValue(t, "objectstore/s3.Store.SetClock: now must not be nil", func() { store.SetClock(nil) })
}
