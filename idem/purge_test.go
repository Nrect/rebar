package idem_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

var purgeNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// countingPruner — idem.Pruner, отдающий по очереди заданные числа удалённых и
// запоминающий аргументы.
type countingPruner struct {
	mu      sync.Mutex
	answers []int
	calls   []purgeCall
	onCall  func()
}

type purgeCall struct {
	before time.Time
	limit  int
}

func (p *countingPruner) Purge(_ context.Context, before time.Time, limit int) (int, error) {
	p.mu.Lock()
	p.calls = append(p.calls, purgeCall{before: before, limit: limit})
	n := 0
	if len(p.calls) <= len(p.answers) {
		n = p.answers[len(p.calls)-1]
	}
	onCall := p.onCall
	p.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	return n, nil
}

func newPurger(t *testing.T, pruner idem.Pruner) *idem.Purger {
	t.Helper()
	p := idem.NewPurger(pruner, testConfig())
	p.SetClock(func() time.Time { return purgeNow })
	return p
}

// Граница — now − Retention, пачка — PurgeBatchSize; прогон идёт до короткой
// пачки.
func TestPurger_RunDeletesInBatchesUntilShort(t *testing.T) {
	t.Parallel()

	pruner := &countingPruner{answers: []int{idem.PurgeBatchSize, idem.PurgeBatchSize, 7}}
	deleted, err := newPurger(t, pruner).Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2*idem.PurgeBatchSize+7, deleted)

	want := purgeCall{before: purgeNow.Add(-idem.MinRetention), limit: idem.PurgeBatchSize}
	assert.Equal(t, []purgeCall{want, want, want}, pruner.calls)
}

func TestPurger_RunStopsOnEmptyBatch(t *testing.T) {
	t.Parallel()

	pruner := &countingPruner{answers: []int{0}}
	deleted, err := newPurger(t, pruner).Run(t.Context())
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Len(t, pruner.calls, 1)
}

// Хранилище, которое всегда отвечает полной пачкой, не держит прогон вечно:
// недоделанное доделывает следующий.
func TestPurger_RunIsBoundedPerRun(t *testing.T) {
	t.Parallel()

	answers := make([]int, idem.PurgeBatchesPerRun+5)
	for i := range answers {
		answers[i] = idem.PurgeBatchSize
	}
	pruner := &countingPruner{answers: answers}
	deleted, err := newPurger(t, pruner).Run(t.Context())
	require.NoError(t, err)
	assert.Len(t, pruner.calls, idem.PurgeBatchesPerRun)
	assert.Equal(t, idem.PurgeBatchesPerRun*idem.PurgeBatchSize, deleted)
}

// Отмена между пачками — причина отмены и число уже удалённых, без похода в
// хранилище и без ErrUnavailable.
func TestPurger_RunStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	pruner := &countingPruner{answers: []int{idem.PurgeBatchSize, idem.PurgeBatchSize}, onCall: cancel}
	deleted, err := newPurger(t, pruner).Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, idem.ErrUnavailable)
	assert.Equal(t, idem.PurgeBatchSize, deleted)
	assert.Len(t, pruner.calls, 1)

	cancelled, stop := context.WithCancel(t.Context())
	stop()
	idle := &countingPruner{}
	_, err = newPurger(t, idle).Run(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, idle.calls, "по отменённому контексту хранилище не зовётся")
}

// Граница считается на каждом прогоне от часов, а не при сборке.
func TestPurger_BoundaryFollowsClock(t *testing.T) {
	t.Parallel()

	pruner := &countingPruner{}
	now := purgeNow
	p := idem.NewPurger(pruner, idem.Config{Operations: []idem.Operation{opCreate}, Retention: 72 * time.Hour, MaxResponseBytes: 1})
	p.SetClock(func() time.Time { return now })
	_, err := p.Run(t.Context())
	require.NoError(t, err)
	now = now.Add(time.Hour)
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []time.Time{purgeNow.Add(-72 * time.Hour), purgeNow.Add(-71 * time.Hour)},
		[]time.Time{pruner.calls[0].before, pruner.calls[1].before})
}

func TestPurger_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "idem.NewPurger: pruner must not be nil", func() { idem.NewPurger(nil, testConfig()) })
	assert.PanicsWithValue(t, "idem.NewPurger: Config.Operations must list at least one operation", func() {
		idem.NewPurger(&countingPruner{}, idem.Config{})
	})
	assert.PanicsWithValue(t, "idem.Purger.SetClock: now must not be nil", func() {
		idem.NewPurger(&countingPruner{}, testConfig()).SetClock(nil)
	})
}
