package mailpg_test

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
)

const testLease = time.Minute

// pendingAt — строка, готовая к отправке в указанный момент.
func pendingAt(t *testing.T, store *mailpg.Store, at time.Time) mail.Envelope {
	t.Helper()
	return mustEnqueue(t, store, envelope(func(e *mail.Envelope) { e.NextAttemptAt = at }))
}

func ids(envs []mail.Envelope) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(envs))
	for _, env := range envs {
		out = append(out, env.ID)
	}
	return out
}

func TestStore_Claim_TakesOnlyDue(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	due := pendingAt(t, store, now.Add(-time.Minute))
	pendingAt(t, store, now.Add(time.Minute))

	claimed, err := store.Claim(context.Background(), now, testLease, 10)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{due.ID}, ids(claimed), "строка из будущего не выдаётся")
	assert.False(t, claimed[0].Reclaimed)
	assert.Equal(t, 1, claimed[0].Attempts)
}

// Строка ровно в next_attempt_at уже должна уходить: граница включительная.
func TestStore_Claim_TakesRowDueExactlyNow(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	due := pendingAt(t, store, now)

	claimed, err := store.Claim(context.Background(), now, testLease, 10)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{due.ID}, ids(claimed))
}

func TestStore_Claim_MarksRowSendingUnderLease(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := pendingAt(t, store, now)

	_, err := store.Claim(context.Background(), now, testLease, 10)
	require.NoError(t, err)

	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusSending), row.Status)
	require.NotNil(t, row.LockedUntil)
	assert.True(t, row.LockedUntil.Equal(now.Add(testLease)), "аренда до now+lease")
	assert.Equal(t, 1, row.Attempts)
	assert.True(t, row.UpdatedAt.Equal(now))
}

// Живая аренда не выдаётся никому; истёкшая — выдаётся с Reclaimed: исход
// прошлой попытки неизвестен, дальше решает Config.Uncertain.
func TestStore_Claim_SkipsLiveLeaseAndReclaimsExpired(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	now := testNow()
	env := pendingAt(t, store, now)
	_, err := store.Claim(ctx, now, testLease, 10)
	require.NoError(t, err)

	live, err := store.Claim(ctx, now.Add(testLease/2), testLease, 10)
	require.NoError(t, err)
	assert.Empty(t, live, "аренда ещё жива")

	expired, err := store.Claim(ctx, now.Add(2*testLease), testLease, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, env.ID, expired[0].ID)
	assert.True(t, expired[0].Reclaimed, "строка пришла из sending с истёкшей арендой")
	assert.Equal(t, 2, expired[0].Attempts)
}

func TestStore_Claim_OrdersByNextAttemptThenID(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	first := pendingAt(t, store, now.Add(-time.Hour))
	sameTime := []uuid.UUID{
		pendingAt(t, store, now.Add(-time.Minute)).ID,
		pendingAt(t, store, now.Add(-time.Minute)).ID,
		pendingAt(t, store, now.Add(-time.Minute)).ID,
	}
	// uuid Postgres сравнивает как 16 байт — тот же порядок, что у bytes.Compare.
	slices.SortFunc(sameTime, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })

	claimed, err := store.Claim(context.Background(), now, testLease, 10)

	require.NoError(t, err)
	assert.Equal(t, append([]uuid.UUID{first.ID}, sameTime...), ids(claimed))
}

func TestStore_Claim_RespectsLimit(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	oldest := pendingAt(t, store, now.Add(-3*time.Hour))
	middle := pendingAt(t, store, now.Add(-2*time.Hour))
	pendingAt(t, store, now.Add(-time.Hour))

	claimed, err := store.Claim(context.Background(), now, testLease, 2)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{oldest.ID, middle.ID}, ids(claimed), "берутся самые старые")
}

// FOR UPDATE SKIP LOCKED: воркеры делят очередь, а не отправляют одно письмо
// дважды.
func TestStore_Claim_TwoWorkersRace(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	now := testNow()
	const (
		rows    = 200
		workers = 4
		batch   = 10
	)
	for range rows {
		pendingAt(t, store, now.Add(-time.Minute))
	}

	var (
		mu    sync.Mutex
		seen  = make(map[uuid.UUID]struct{}, rows)
		total int
		wg    sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				claimed, err := store.Claim(ctx, now, time.Hour, batch)
				if !assert.NoError(t, err) || len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, env := range claimed {
					seen[env.ID] = struct{}{}
					total++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, rows, total, "выдано ровно столько строк, сколько было")
	assert.Len(t, seen, rows, "пересечений между воркерами нет")
}
