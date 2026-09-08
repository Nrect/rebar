package schedulertest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/scheduler/schedulertest"
)

// Часы стоят там, куда их поставили, и двигаются только руками: тест на
// длительность прогона иначе был бы тестом на скорость машины.
func TestFakeClock_AdvanceAndSet(t *testing.T) {
	t.Parallel()

	c := schedulertest.NewFakeClock(at)
	assert.Equal(t, at, c.Now())

	c.Advance(90 * time.Second)
	assert.Equal(t, at.Add(90*time.Second), c.Now())

	c.Advance(-30 * time.Second)
	assert.Equal(t, at.Add(60*time.Second), c.Now(), "назад тоже двигаются: тест вправе смоделировать перевод часов")

	c.Set(at)
	assert.Equal(t, at, c.Now())
}

// Now зовут горутины задач, Advance — тест: -race обязан быть чистым.
func TestFakeClock_IsRaceFree(t *testing.T) {
	t.Parallel()

	c := schedulertest.NewFakeClock(at)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Advance(time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, at.Add(400*time.Millisecond), c.Now())
}
