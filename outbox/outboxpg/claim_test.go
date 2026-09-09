package outboxpg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

func TestStore_Claim_TakesDueRowsUnderLease(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	env := mustEnqueue(t, store, envelopeAt(now))

	claimed, token := mustClaim(t, store, now, 10)

	require.Len(t, claimed, 1)
	assert.Equal(t, env.ID, claimed[0].ID)
	assert.Equal(t, outbox.StatusProcessing, claimed[0].Status)
	assert.Equal(t, 1, claimed[0].Attempts, "попытка считается при захвате")
	assert.False(t, claimed[0].Reclaimed)
	require.NotNil(t, claimed[0].ClaimToken)
	assert.Equal(t, token, *claimed[0].ClaimToken)

	got := readRow(t, pool, env.ID)
	assert.Equal(t, string(outbox.StatusProcessing), got.Status)
	require.NotNil(t, got.LockedUntil)
	assert.Equal(t, now.Add(time.Minute).UTC(), got.LockedUntil.UTC())
}

// Отложенная строка не берётся раньше срока: NotBefore — это предикат, а не
// подсказка.
func TestStore_Claim_SkipsDeferredRows(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(time.Hour) }))

	early, _ := mustClaim(t, store, now, 10)
	late, _ := mustClaim(t, store, now.Add(time.Hour), 10)

	assert.Empty(t, early)
	assert.Len(t, late, 1)
}

// Строка с живой арендой занята другим прогоном; с истёкшей — воркер упал, и
// она выдаётся снова с признаком перехвата.
func TestStore_Claim_ReclaimsExpiredLeaseOnly(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	env := mustEnqueue(t, store, envelopeAt(now))
	first, _ := mustClaim(t, store, now, 10)
	require.Len(t, first, 1)

	alive, _ := mustClaim(t, store, now.Add(30*time.Second), 10)
	assert.Empty(t, alive, "живая аренда строку не отдаёт")

	expired, _ := mustClaim(t, store, now.Add(2*time.Minute), 10)
	require.Len(t, expired, 1)
	assert.Equal(t, env.ID, expired[0].ID)
	assert.True(t, expired[0].Reclaimed, "исход прошлой попытки неизвестен")
	assert.Equal(t, 2, expired[0].Attempts)
}

// Фильтр Claim — реестр воркера: при выкате старый инстанс не заберёт то, что
// умеет только новый, и не утопит его в dead-letter.
func TestStore_Claim_SkipsUnknownKinds(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	known := mustEnqueue(t, store, envelopeAt(now))
	unknown := mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) { e.Kind = secondKind }))

	claimed, _ := mustClaim(t, store, now, 10, testKind)

	require.Len(t, claimed, 1)
	assert.Equal(t, known.ID, claimed[0].ID)

	both, _ := mustClaim(t, store, now, 10, testKind, secondKind)
	require.Len(t, both, 1, "первая строка уже под арендой")
	assert.Equal(t, unknown.ID, both[0].ID)
}

// Пустой список типов и непозитивный лимит — пустая выборка БЕЗ ошибки:
// ошибка Claim остановила бы прогон, а «мне нечего забирать» не сбой.
func TestStore_Claim_EmptyRequestIsNotAnError(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	mustEnqueue(t, store, envelopeAt(now))

	tests := []struct {
		name  string
		limit int
		kinds []outbox.Kind
	}{
		{name: "лимит ноль", limit: 0, kinds: []outbox.Kind{testKind}},
		{name: "лимит отрицательный", limit: -1, kinds: []outbox.Kind{testKind}},
		{name: "типов нет", limit: 10, kinds: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			claimed, err := store.Claim(t.Context(), outbox.ClaimRequest{
				Now: now, Lease: time.Minute, Limit: tt.limit, Kinds: tt.kinds, Token: uuid.New(),
			})
			require.NoError(t, err)
			assert.Empty(t, claimed)
		})
	}
}

func TestStore_Claim_RespectsLimitAndOrder(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	for i := range 5 {
		mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) {
			e.AvailableAt = now.Add(-time.Duration(i) * time.Minute)
		}))
	}

	claimed, _ := mustClaim(t, store, now, 2)

	require.Len(t, claimed, 2)
	assert.False(t, claimed[0].AvailableAt.After(claimed[1].AvailableAt), "самые старые первыми")
}

// Арбитр — база, а не Go: между «посмотреть» и «взять» помещается чужая
// транзакция, поэтому строку раздаёт FOR UPDATE SKIP LOCKED.
func TestStore_Claim_FourWorkersRace(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	const rows = 40
	for range rows {
		mustEnqueue(t, store, envelopeAt(now))
	}

	const workers = 4
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got = map[uuid.UUID]int{}
	)
	wg.Add(workers) // Add до go: иначе -race мигает через раз на самом тесте
	for range workers {
		go func() {
			defer wg.Done()
			claimed, err := outboxpg.New(pool).Claim(t.Context(), outbox.ClaimRequest{
				Now: now, Lease: time.Minute, Limit: rows,
				Kinds: []outbox.Kind{testKind}, Token: uuid.New(),
			})
			assert.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			for _, env := range claimed {
				got[env.ID]++
			}
		}()
	}
	wg.Wait()

	assert.Len(t, got, rows, "каждая строка досталась ровно одному воркеру")
	for id, times := range got {
		assert.Equal(t, 1, times, "строка %s выдана дважды", id)
	}
	assert.Equal(t, rows,
		countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE status = 'processing' AND attempts = 1`))
}
