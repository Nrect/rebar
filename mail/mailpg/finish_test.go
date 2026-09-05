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

// sending — строка, взятая воркером: состояние, из которого только и можно
// записать исход.
func sending(t *testing.T, store *mailpg.Store, now time.Time) mail.Envelope {
	t.Helper()
	env := pendingAt(t, store, now)
	claimed, err := store.Claim(context.Background(), now, testLease, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	return env
}

func TestStore_Finish_Sent(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := sending(t, store, now)
	done := now.Add(2 * time.Second)

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishSent, Now: done,
		Transport: "smtp", ProviderMessageID: "queued-as-42",
	})

	require.NoError(t, err)
	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusSent), row.Status)
	assert.Empty(t, row.Subject)
	assert.Empty(t, row.Text)
	assert.Empty(t, row.HTML)
	assert.Empty(t, row.Headers)
	assert.Nil(t, row.LockedUntil)
	require.NotNil(t, row.SentAt)
	assert.True(t, row.SentAt.Equal(done))
	assert.Equal(t, "smtp", row.Transport)
	assert.Equal(t, "queued-as-42", row.ProviderMessageID)
	assert.Empty(t, row.FailReason)
	assert.True(t, row.UpdatedAt.Equal(done))
}

// Ретрай возвращает строку в очередь и тело не трогает: письмо ещё поедет.
func TestStore_Finish_Retry(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := sending(t, store, now)
	done := now.Add(2 * time.Second)
	next := now.Add(5 * time.Minute)

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishRetry, Now: done, NextAttemptAt: next,
		Error: "dial tcp: i/o timeout", Transport: "smtp",
	})

	require.NoError(t, err)
	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusPending), row.Status)
	assert.Nil(t, row.LockedUntil)
	assert.True(t, row.NextAttemptAt.Equal(next))
	assert.Equal(t, "dial tcp: i/o timeout", row.LastError)
	assert.Equal(t, "smtp", row.Transport)
	assert.Equal(t, env.Subject, row.Subject)
	assert.Equal(t, env.Text, row.Text)
	assert.Equal(t, 1, row.Attempts, "попытку считает Claim, а не Finish")
	assert.Nil(t, row.SentAt)
}

func TestStore_Finish_TerminalOutcomesClearBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		outcome        mail.FinishOutcome
		failReason     mail.FailReason
		wantStatus     mail.Status
		wantFailReason string
	}{
		{
			name: "failed", outcome: mail.FinishFailed, failReason: mail.FailRejected,
			wantStatus: mail.StatusFailed, wantFailReason: string(mail.FailRejected),
		},
		{
			name: "expired", outcome: mail.FinishExpired, failReason: mail.FailExhausted,
			wantStatus: mail.StatusExpired, wantFailReason: "",
		},
		{
			name: "suppressed", outcome: mail.FinishSuppressed,
			wantStatus: mail.StatusSuppressed, wantFailReason: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			now := testNow()
			env := sending(t, store, now)
			done := now.Add(time.Second)

			err := store.Finish(context.Background(), mail.FinishRequest{
				ID: env.ID, Outcome: tt.outcome, Now: done,
				Error: "провайдер отказал", FailReason: tt.failReason, Transport: "sesv2",
			})

			require.NoError(t, err)
			row := readRow(t, pool, env.ID)
			assert.Equal(t, string(tt.wantStatus), row.Status)
			assert.Equal(t, tt.wantFailReason, row.FailReason, "причина — только у failed")
			assert.Empty(t, row.Subject)
			assert.Empty(t, row.Text)
			assert.Empty(t, row.HTML)
			assert.Empty(t, row.Headers)
			assert.Nil(t, row.LockedUntil)
			assert.Nil(t, row.SentAt, "sent_at ставится только успеху")
			assert.Equal(t, "провайдер отказал", row.LastError)
		})
	}
}

// Ноль обновлённых строк — не успех: аренду забрал другой воркер, и исход
// записывать некуда.
func TestStore_Finish_NotSendingIsUnavailable(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := mustEnqueue(t, store, envelope())

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishSent, Now: now, Transport: "smtp",
	})

	require.Error(t, err)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusPending), row.Status)
	assert.Equal(t, env.Text, row.Text, "чужой исход строку не трогает")
}

func TestStore_Finish_UnknownRowIsUnavailable(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: envelope().ID, Outcome: mail.FinishSent, Now: testNow(), Transport: "smtp",
	})

	assert.ErrorIs(t, err, mail.ErrUnavailable)
}

func TestStore_Finish_UnknownOutcomeIsRefused(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := sending(t, store, now)

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishOutcome("teleported"), Now: now,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Equal(t, string(mail.StatusSending), readRow(t, pool, env.ID).Status)
}

// Утечка Detail: FailReason вне CHECK ловится базой. Этот UPDATE стирает тело
// в том же запросе, поэтому «Failing row contains» здесь уже без письма —
// строка с телом проверяется на INSERT (TestStore_Enqueue_ErrorHidesBody).
func TestStore_Finish_BadFailReasonHidesBody(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	env := sending(t, store, now)

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishFailed, Now: now,
		FailReason: mail.FailReason("бесовщина"), Transport: "smtp",
	})

	require.Error(t, err)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Contains(t, err.Error(), "23514")
	assert.NotContains(t, err.Error(), secretLink)
	assert.NotContains(t, err.Error(), "Failing row")
	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusSending), row.Status, "отвергнутый UPDATE строку не поменял")
	assert.Equal(t, env.Text, row.Text)
}
