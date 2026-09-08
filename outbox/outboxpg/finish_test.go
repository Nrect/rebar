package outboxpg_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Исход попытки в строку. Payload не стирается ни в одном из них, включая
// failed: без него redrive невозможен.
func TestStore_Finish_WritesOutcome(t *testing.T) {
	t.Parallel()
	now := pgtest.Now()
	next := now.Add(5 * time.Minute)

	tests := []struct {
		name       string
		req        outbox.FinishRequest
		wantStatus outbox.Status
		assert     func(t *testing.T, got row)
	}{
		{
			name:       "done",
			req:        outbox.FinishRequest{Outcome: outbox.FinishDone},
			wantStatus: outbox.StatusDone,
			assert: func(t *testing.T, got row) {
				require.NotNil(t, got.DoneAt)
				assert.Equal(t, now, got.DoneAt.UTC())
			},
		},
		{
			name:       "skipped — для строки тот же done",
			req:        outbox.FinishRequest{Outcome: outbox.FinishSkipped},
			wantStatus: outbox.StatusDone,
			assert:     func(t *testing.T, got row) { require.NotNil(t, got.DoneAt) },
		},
		{
			name:       "retry",
			req:        outbox.FinishRequest{Outcome: outbox.FinishRetry, NextAttemptAt: next, Error: "boom"},
			wantStatus: outbox.StatusPending,
			assert: func(t *testing.T, got row) {
				assert.Equal(t, next, got.AvailableAt.UTC())
				assert.Equal(t, "boom", got.LastError)
				assert.Equal(t, 1, got.Attempts, "попытка потрачена")
			},
		},
		{
			name:       "failed(permanent)",
			req:        outbox.FinishRequest{Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent, Error: "нет такого счёта"},
			wantStatus: outbox.StatusFailed,
			assert: func(t *testing.T, got row) {
				assert.Equal(t, string(outbox.FailPermanent), got.FailReason)
				assert.NotEmpty(t, got.Payload, "payload остаётся: без него нечем делать redrive")
			},
		},
		{
			name:       "failed(exhausted)",
			req:        outbox.FinishRequest{Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted},
			wantStatus: outbox.StatusFailed,
			assert: func(t *testing.T, got row) {
				assert.Equal(t, string(outbox.FailExhausted), got.FailReason)
			},
		},
		{
			name:       "expired",
			req:        outbox.FinishRequest{Outcome: outbox.FinishExpired},
			wantStatus: outbox.StatusExpired,
			assert:     func(t *testing.T, got row) { assert.Empty(t, got.FailReason) },
		},
		{
			name:       "released — попытка возвращается",
			req:        outbox.FinishRequest{Outcome: outbox.FinishReleased},
			wantStatus: outbox.StatusPending,
			assert: func(t *testing.T, got row) {
				assert.Zero(t, got.Attempts, "быстрая остановка не жжёт лимит попыток")
				assert.Equal(t, now, got.AvailableAt.UTC(), "строка готова немедленно")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			env := mustEnqueue(t, store, envelopeAt(now))
			claimed, token := mustClaim(t, store, now, 10)
			require.Len(t, claimed, 1)

			req := tt.req
			req.ID, req.Token, req.Now = env.ID, token, now
			require.NoError(t, store.Finish(t.Context(), req))

			got := readRow(t, pool, env.ID)
			assert.Equal(t, string(tt.wantStatus), got.Status)
			assert.Nil(t, got.ClaimToken, "аренда снята во всех исходах")
			assert.Nil(t, got.LockedUntil)
			assert.NotEmpty(t, got.Payload, "payload не стирается")
			tt.assert(t, got)
		})
	}
}

// Fencing: воркер A, проснувшийся после паузы GC, не перепишет результат
// воркера B. Ноль строк — ErrClaimLost и НИ ОДНОГО изменения.
func TestStore_Finish_StaleTokenAffectsNoRows(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := pgtest.Now()
	env := mustEnqueue(t, store, envelopeAt(now))

	stale, staleToken := mustClaim(t, store, now, 10)
	require.Len(t, stale, 1)
	// Аренда истекла, строку перезабрал сосед под своим токеном.
	fresh, freshToken := mustClaim(t, store, now.Add(2*time.Minute), 10)
	require.Len(t, fresh, 1)
	require.NotEqual(t, staleToken, freshToken)
	before := readRow(t, pool, env.ID)

	err := store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: staleToken, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	})

	require.ErrorIs(t, err, outbox.ErrClaimLost)
	assert.Equal(t, before, readRow(t, pool, env.ID), "строка не изменилась ни в одном поле")

	// Живой токен свою же строку закрывает.
	require.NoError(t, store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: freshToken, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	}))
	assert.Equal(t, string(outbox.StatusDone), readRow(t, pool, env.ID).Status)
}

// Строка не в processing (уже закрыта) — тот же ErrClaimLost: записывать
// исход некуда, и это не успех.
func TestStore_Finish_NotProcessingIsClaimLost(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	env := mustEnqueue(t, store, envelopeAt(now))

	tests := []struct {
		name  string
		id    uuid.UUID
		token uuid.UUID
	}{
		{name: "строка ещё в pending", id: env.ID, token: uuid.New()},
		{name: "строки нет вовсе", id: uuid.New(), token: uuid.New()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := store.Finish(t.Context(), outbox.FinishRequest{
				ID: tt.id, Token: tt.token, Outcome: outbox.FinishDone, Now: now,
			})
			require.ErrorIs(t, err, outbox.ErrClaimLost)
		})
	}
}

// Неизвестный исход — отказ, а не молчаливое «ну и ладно»: строка осталась бы
// под арендой навсегда.
func TestStore_Finish_UnknownOutcomeIsUnavailable(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	now := pgtest.Now()
	env := mustEnqueue(t, store, envelopeAt(now))
	_, token := mustClaim(t, store, now, 10)

	err := store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishOutcome("bogus"), Now: now,
	})

	require.ErrorIs(t, err, outbox.ErrUnavailable)
}

// Аренду держит база, а не только код: правку в обход адаптера отвергает CHECK.
func TestSchema_ChecksGuardLeaseAndDeadLetter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		update     string
		constraint string
	}{
		{
			name:       "processing без токена и срока",
			update:     `UPDATE outbox_messages SET status = 'processing' WHERE id = $1`,
			constraint: "outbox_messages_claim_chk",
		},
		{
			name:       "аренда не у processing",
			update:     `UPDATE outbox_messages SET claim_token = gen_random_uuid(), locked_until = now() WHERE id = $1`,
			constraint: "outbox_messages_claim_chk",
		},
		{
			name:       "failed без причины",
			update:     `UPDATE outbox_messages SET status = 'failed' WHERE id = $1`,
			constraint: "outbox_messages_fail_chk",
		},
		{
			name:       "причина не у failed",
			update:     `UPDATE outbox_messages SET fail_reason = 'permanent' WHERE id = $1`,
			constraint: "outbox_messages_fail_chk",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			env := mustEnqueue(t, store, envelope())

			_, err := pool.Exec(t.Context(), tt.update, env.ID)

			require.Error(t, err)
			assertConstraintViolation(t, err, tt.constraint)
		})
	}
}
