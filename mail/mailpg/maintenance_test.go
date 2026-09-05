package mailpg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
)

// finished — строка в терминальном статусе с заданным updated_at.
func finished(t *testing.T, store *mailpg.Store, now, done time.Time, outcome mail.FinishOutcome) mail.Envelope {
	t.Helper()
	env := sending(t, store, now)
	require.NoError(t, store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: outcome, Now: done, FailReason: mail.FailExhausted, Transport: "smtp",
	}))
	return env
}

func TestStore_Stats_Empty(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	stats, err := store.Stats(context.Background(), testNow())

	require.NoError(t, err)
	assert.Equal(t, mail.Stats{}, stats)
}

func TestStore_Stats_CountsQueueAndFailed(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	// sending считается вместе с pending: письмо всё ещё не ушло.
	sending(t, store, now.Add(-time.Minute))
	finished(t, store, now.Add(-time.Minute), now, mail.FinishFailed)
	finished(t, store, now.Add(-time.Minute), now, mail.FinishSent)
	oldest := mustEnqueue(t, store, envelope(func(e *mail.Envelope) {
		e.CreatedAt = now.Add(-10 * time.Minute)
		e.NextAttemptAt = now.Add(time.Hour) // не due: Claim выше его не заберёт
	}))
	mustEnqueue(t, store, envelope(func(e *mail.Envelope) {
		e.CreatedAt = now.Add(-time.Minute)
		e.NextAttemptAt = now.Add(time.Hour)
	}))

	stats, err := store.Stats(context.Background(), now)

	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Pending, "два pending и один sending")
	assert.Equal(t, int64(1), stats.Failed)
	assert.Equal(t, now.Sub(oldest.CreatedAt), stats.OldestPendingAge, "возраст по created_at")
}

// Часы потребителя и строк могут разъехаться: гейдж не показывает минус.
func TestStore_Stats_FutureRowGivesZeroAge(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := testNow()
	mustEnqueue(t, store, envelope(func(e *mail.Envelope) { e.CreatedAt = now.Add(time.Hour) }))

	stats, err := store.Stats(context.Background(), now)

	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Pending)
	assert.Zero(t, stats.OldestPendingAge)
}

func TestStore_Purge_DeletesOnlyOldTerminal(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := context.Background()
	now := testNow()
	old := finished(t, store, now.Add(-2*time.Hour), now.Add(-2*time.Hour), mail.FinishSent)
	fresh := finished(t, store, now.Add(-time.Minute), now.Add(-time.Minute), mail.FinishFailed)
	queued := mustEnqueue(t, store, envelope())

	deleted, err := store.Purge(ctx, now.Add(-time.Hour), 10)

	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, old.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, fresh.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, queued.ID))
}

// Граница строгая: строка с updated_at ровно before остаётся.
func TestStore_Purge_BorderIsExclusive(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := context.Background()
	now := testNow()
	done := now.Add(-time.Hour)
	env := finished(t, store, now.Add(-2*time.Hour), done, mail.FinishSent)

	deleted, err := store.Purge(ctx, done, 10)
	require.NoError(t, err)
	assert.Zero(t, deleted)

	deleted, err = store.Purge(ctx, done.Add(time.Microsecond), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, env.ID))
}

func TestStore_Purge_RespectsLimit(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	for range 3 {
		finished(t, store, now.Add(-2*time.Hour), now.Add(-2*time.Hour), mail.FinishSent)
	}

	deleted, err := store.Purge(context.Background(), now, 2)

	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM email_outbox`))
}
