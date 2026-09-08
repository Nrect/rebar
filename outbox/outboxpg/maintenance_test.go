package outboxpg_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Возраст, а не глубина: «воркер жив, но ничего не уходит» глубиной не
// ловится. Отложенные и арендованные строки в возраст не входят, иначе гейдж
// горел бы от штатной задержки и алерт «очередь встала» выключили бы.
func TestStore_Stats_OldestDueIgnoresDeferredAndClaimed(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()

	// Самая старая строка — под арендой: она не «стоит», её уже взяли.
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) {
		e.AvailableAt = now.Add(-time.Hour)
	}))
	claimed, _ := mustClaim(t, store, now, 1)
	require.Len(t, claimed, 1)

	// Ещё старше, но отложена: её срок не наступил.
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) {
		e.AvailableAt = now.Add(2 * time.Hour)
	}))
	// Единственная готовая строка — она и задаёт возраст.
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) {
		e.AvailableAt = now.Add(-10 * time.Minute)
	}))

	stats, err := store.Stats(t.Context(), now, []outbox.Kind{testKind})

	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Pending, "отложенная строка тоже pending")
	assert.Equal(t, int64(1), stats.Processing)
	assert.Zero(t, stats.Failed)
	assert.Zero(t, stats.Unhandled)
	assert.Equal(t, 10*time.Minute, stats.OldestDueAge)
}

// Готовых строк нет — возраст нулевой, а не «очень большой».
func TestStore_Stats_NoDueRowsGivesZeroAge(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) {
		e.AvailableAt = now.Add(time.Hour)
	}))

	stats, err := store.Stats(t.Context(), now, []outbox.Kind{testKind})

	require.NoError(t, err)
	assert.Zero(t, stats.OldestDueAge)
	assert.Equal(t, int64(1), stats.Pending)
}

// Unhandled считается по реестру ЭТОГО воркера: строку с типом, которого он не
// умеет, не возьмёт никто из его инстансов, и это отдельный алерт.
func TestStore_Stats_UnhandledCountsKindsOutsideRegistry(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	mustEnqueue(t, store, envelopeAt(now))
	mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) { e.Kind = secondKind }))

	known, err := store.Stats(t.Context(), now, []outbox.Kind{testKind})
	require.NoError(t, err)
	assert.Equal(t, int64(1), known.Unhandled, "второй тип этот воркер не умеет")

	both, err := store.Stats(t.Context(), now, []outbox.Kind{testKind, secondKind})
	require.NoError(t, err)
	assert.Zero(t, both.Unhandled)

	// Пустой список известных типов — необслуженными оказываются все: пустой
	// массив в SQL обязан остаться массивом, а не превратиться в NULL.
	none, err := store.Stats(t.Context(), now, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), none.Unhandled)
}

// failed НЕ ЧИСТИТСЯ НИКОГДА: молча исчезнувший dead-letter — это потерянное
// событие без следов.
func TestStore_Purge_KeepsFailed(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	done := finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})
	expired := finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishExpired})
	failed := finishedRow(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})
	pending := mustEnqueue(t, store, envelopeAt(now))

	deleted, err := store.Purge(t.Context(), now.Add(time.Hour), 100)

	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, done.ID))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, expired.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, failed.ID),
		"dead-letter переживает любой ретеншн")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, pending.ID))

	// payload dead-letter цел: без него redrive невозможен.
	assert.NotEmpty(t, readRow(t, pool, failed.ID).Payload)
}

func TestStore_Purge_HonoursHorizonAndLimit(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	for range 3 {
		finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})
	}

	fresh, err := store.Purge(t.Context(), now, 100)
	require.NoError(t, err)
	assert.Zero(t, fresh, "строки моложе горизонта не трогаются")

	limited, err := store.Purge(t.Context(), now.Add(time.Hour), 2)
	require.NoError(t, err)
	assert.Equal(t, 2, limited)

	none, err := store.Purge(t.Context(), now.Add(time.Hour), 0)
	require.NoError(t, err)
	assert.Zero(t, none, "непозитивный лимит — ноль без ошибки")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

// Redrive — операция оператора, и только из failed: воскрешать done или
// перехватывать чужую аренду она не вправе.
func TestStore_Redrive_OnlyFromFailed(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	later := now.Add(time.Hour)

	failed := finishedRow(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent, Error: "нет такого счёта",
	})
	done := finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})
	expired := finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishExpired})
	pending := mustEnqueue(t, store, envelopeAt(now))
	processing := mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) { e.DedupKey = "" }))
	_, _ = mustClaim(t, store, now, 10)

	ok, err := store.Redrive(t.Context(), failed.ID, later)
	require.NoError(t, err)
	assert.True(t, ok)
	got := readRow(t, pool, failed.ID)
	assert.Equal(t, string(outbox.StatusPending), got.Status)
	assert.Zero(t, got.Attempts, "попытки сброшены")
	assert.Empty(t, got.FailReason)
	assert.Equal(t, later, got.AvailableAt.UTC())
	assert.Equal(t, "нет такого счёта", got.LastError,
		"причина сохранена: оператору видно, из-за чего строка попала в dead-letter")

	for _, tt := range []struct {
		name string
		id   uuid.UUID
	}{
		{name: "done", id: done.ID},
		{name: "expired", id: expired.ID},
		{name: "pending", id: pending.ID},
		{name: "processing", id: processing.ID},
		{name: "строки нет", id: uuid.New()},
		{name: "повторный клик по уже возвращённой", id: failed.ID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			affected, redriveErr := store.Redrive(t.Context(), tt.id, later)
			require.NoError(t, redriveErr, "«не сработало» — не ошибка")
			assert.False(t, affected)
		})
	}
	assert.Equal(t, string(outbox.StatusDone), readRow(t, pool, done.ID).Status)
}

// ListFailed — dead-letter для оператора, самые старые первыми.
func TestStore_ListFailed_OldestFirst(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	first := finishedRow(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent,
	})
	second := finishedRow(t, store, now.Add(time.Minute), outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})
	finishedRow(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})

	failed, err := store.ListFailed(t.Context(), 10)

	require.NoError(t, err)
	require.Len(t, failed, 2)
	assert.Equal(t, first.ID, failed[0].ID)
	assert.Equal(t, second.ID, failed[1].ID)
	assert.NotEmpty(t, failed[0].Payload, "payload доступен оператору")

	limited, err := store.ListFailed(t.Context(), 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	empty, err := store.ListFailed(t.Context(), 0)
	require.NoError(t, err, "непозитивный лимит — пустая выборка без ошибки")
	assert.Empty(t, empty)
}

func TestNew_PanicsOnNilPool(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { outboxpg.New(nil) })
}

func TestWithTx_PanicsOnNilTx(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { new(outboxpg.Store).WithTx(nil) })
}
