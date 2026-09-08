package outboxtest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

var at = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func row(id uuid.UUID, kind outbox.Kind, key string) outbox.Envelope {
	return outbox.Envelope{
		ID: id, Kind: kind, DedupKey: key,
		Payload:     json.RawMessage(`{"a":1}`),
		Fingerprint: []byte{1, 2, 3},
		Status:      outbox.StatusPending,
		AvailableAt: at, OccurredAt: at, CreatedAt: at, UpdatedAt: at,
		SchemaVersion: 1,
	}
}

func claim(t *testing.T, store *outboxtest.MemStore, token uuid.UUID) []outbox.Envelope {
	t.Helper()

	claimed, err := store.Claim(context.Background(), outbox.ClaimRequest{
		Now: at, Lease: time.Minute, Limit: 10,
		Kinds: []outbox.Kind{"order.paid"}, Token: token,
	})
	require.NoError(t, err)
	return claimed
}

func TestMemStore_DedupIsPerKindAndKey(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	ctx := context.Background()

	first, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", "k"))
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, first.Outcome)

	dup, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", "k"))
	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeDuplicate, dup.Outcome)
	assert.Equal(t, first.Envelope.ID, dup.Envelope.ID)
	assert.Equal(t, first.Envelope.Fingerprint, dup.Envelope.Fingerprint, "отпечаток возвращается байт в байт")

	otherKind, err := store.Enqueue(ctx, row(uuid.New(), "receipt.send", "k"))
	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, otherKind.Outcome)

	noKey, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", ""))
	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, noKey.Outcome)
	noKeyAgain, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", ""))
	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, noKeyAgain.Outcome, "пустой ключ дедупу не подлежит")

	_, err = store.Enqueue(ctx, row(first.Envelope.ID, "receipt.send", "other"))
	require.ErrorIs(t, err, outboxtest.ErrIDReused, "ошибка двойника отличима от доменной")
}

// Живая аренда — строка занята; истёкшая — возвращается с Reclaimed.
func TestMemStore_LeaseHidesRowUntilItExpires(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	env := row(uuid.New(), "order.paid", "k")
	_, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)

	first := claim(t, store, uuid.New())
	require.Len(t, first, 1)
	assert.False(t, first[0].Reclaimed)
	assert.Equal(t, 1, first[0].Attempts)
	require.NotNil(t, first[0].ClaimToken)

	assert.Empty(t, claim(t, store, uuid.New()), "живая аренда строку прячет")

	claimed, err := store.Claim(context.Background(), outbox.ClaimRequest{
		Now: at.Add(2 * time.Minute), Lease: time.Minute, Limit: 10,
		Kinds: []outbox.Kind{"order.paid"}, Token: uuid.New(),
	})
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.True(t, claimed[0].Reclaimed, "истёкшая аренда — исход прошлой попытки неизвестен")
	assert.Equal(t, 2, claimed[0].Attempts)
}

func TestMemStore_ClaimIsFilteredAndBounded(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	ctx := context.Background()
	_, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", "a"))
	require.NoError(t, err)
	_, err = store.Enqueue(ctx, row(uuid.New(), "receipt.send", "b"))
	require.NoError(t, err)

	assert.Len(t, claim(t, store, uuid.New()), 1, "чужой Kind не забирается")

	empty, err := store.Claim(ctx, outbox.ClaimRequest{Now: at, Lease: time.Minute, Limit: 0})
	require.NoError(t, err, "непозитивный лимит — не ошибка")
	assert.Empty(t, empty)

	empty, err = store.Claim(ctx, outbox.ClaimRequest{Now: at, Lease: time.Minute, Limit: 10})
	require.NoError(t, err, "пустой список типов — не ошибка")
	assert.Empty(t, empty)
}

// Fencing: Finish с чужим токеном не меняет ничего.
func TestMemStore_FinishRequiresOwnToken(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	env := row(uuid.New(), "order.paid", "k")
	_, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)

	token := uuid.New()
	require.Len(t, claim(t, store, token), 1)

	err = store.Finish(context.Background(), outbox.FinishRequest{
		ID: env.ID, Token: uuid.New(), Outcome: outbox.FinishDone, Now: at,
	})
	require.ErrorIs(t, err, outbox.ErrClaimLost)
	stored, _ := store.Get(env.ID)
	assert.Equal(t, outbox.StatusProcessing, stored.Status, "состояние не менялось")

	require.NoError(t, store.Finish(context.Background(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishDone, Now: at,
	}))
	stored, _ = store.Get(env.ID)
	assert.Equal(t, outbox.StatusDone, stored.Status)
	assert.Nil(t, stored.ClaimToken)
	assert.NotEmpty(t, stored.Payload, "payload не стирается ни в одном исходе")

	err = store.Finish(context.Background(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishDone, Now: at,
	})
	require.ErrorIs(t, err, outbox.ErrClaimLost, "строка уже не в processing")
}

func TestMemStore_ReleasedGivesTheAttemptBack(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	env := row(uuid.New(), "order.paid", "k")
	_, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)

	token := uuid.New()
	require.Len(t, claim(t, store, token), 1)
	require.NoError(t, store.Finish(context.Background(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishReleased, Now: at,
	}))

	stored, _ := store.Get(env.ID)
	assert.Equal(t, outbox.StatusPending, stored.Status)
	assert.Equal(t, 0, stored.Attempts)
	assert.True(t, stored.AvailableAt.Equal(at))
}

// Отменённый контекст двойник замечает: иначе он зеленил бы код, который
// записывает исход «после остановки», а настоящий драйвер этого не сделает.
func TestMemStore_RespectsCancelledContext(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.Enqueue(ctx, row(uuid.New(), "order.paid", "k"))
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Claim(ctx, outbox.ClaimRequest{Now: at, Lease: time.Minute, Limit: 1, Kinds: []outbox.Kind{"order.paid"}, Token: uuid.New()})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.Finish(ctx, outbox.FinishRequest{}), context.Canceled)
	_, err = store.Stats(ctx, at, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Purge(ctx, at, 1)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.ListFailed(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Redrive(ctx, uuid.New(), at)
	require.ErrorIs(t, err, context.Canceled)
}

// Снимки отдаются копиями: тест не должен править внутренности хранилища.
func TestMemStore_SnapshotsAreCopies(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	env := row(uuid.New(), "order.paid", "k")
	env.Headers = map[string]string{"traceparent": "00-abc-01"}
	_, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)

	got, ok := store.Get(env.ID)
	require.True(t, ok)
	got.Headers["traceparent"] = "подменён"
	got.Payload[0] = 'X'
	got.Fingerprint[0] = 9

	fresh, _ := store.Get(env.ID)
	assert.Equal(t, "00-abc-01", fresh.Headers["traceparent"])
	assert.JSONEq(t, `{"a":1}`, string(fresh.Payload))
	assert.Equal(t, []byte{1, 2, 3}, fresh.Fingerprint)
	assert.Len(t, store.Rows(), 1)
}

func TestRecordingHandler_AnswersByAggregateThenKind(t *testing.T) {
	t.Parallel()
	h := outboxtest.NewRecordingHandler()
	h.PermanentFor["A-1"] = true
	h.FailFor["order.paid"] = 1
	h.SkipFor["A-2"] = true
	h.ThrottleFor["A-3"] = time.Minute
	ctx := context.Background()

	require.True(t, outbox.IsPermanent(h.Handle(ctx, outbox.Delivery{Kind: "order.paid", AggregateID: "A-1"})))
	require.ErrorIs(t, h.Handle(ctx, outbox.Delivery{Kind: "order.paid", AggregateID: "A-2"}), outbox.ErrSkip)

	after, named := outbox.RetryAfterOf(h.Handle(ctx, outbox.Delivery{Kind: "order.paid", AggregateID: "A-3"}))
	assert.True(t, named)
	assert.Equal(t, time.Minute, after)

	// Без AggregateID ключом становится Kind; счётчик FailFor исчерпывается.
	require.ErrorIs(t, h.Handle(ctx, outbox.Delivery{Kind: "order.paid"}), outboxtest.ErrHandlerFailed)
	require.NoError(t, h.Handle(ctx, outbox.Delivery{Kind: "order.paid"}))

	assert.Len(t, h.Handled(), 5, "доставка записывается и при отказе")
}

func TestRecordingHandler_PanicsOnDemand(t *testing.T) {
	t.Parallel()
	h := outboxtest.NewRecordingHandler()
	h.PanicFor["A-1"] = 1
	d := outbox.Delivery{Kind: "order.paid", AggregateID: "A-1"}

	assert.Panics(t, func() { _ = h.Handle(context.Background(), d) })
	assert.Equal(t, 1, h.Panicked("A-1"))
	require.NoError(t, h.Handle(context.Background(), d), "паника разовая")
}

func TestClock_Advances(t *testing.T) {
	t.Parallel()
	c := outboxtest.NewClock(at)
	assert.Equal(t, at, c.Now())

	c.Advance(time.Hour)
	assert.Equal(t, at.Add(time.Hour), c.Now())

	c.Set(at.In(time.FixedZone("MSK", 3*60*60)))
	assert.Equal(t, time.UTC, c.Now().Location(), "часы двойника в UTC, как и ядро")
}
