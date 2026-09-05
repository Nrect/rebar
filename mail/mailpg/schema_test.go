package mailpg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Тело стирает не только код: правку в обход адаптера отвергает сама база.
func TestSchema_ChecksGuardTerminalRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		update     string
		constraint string
	}{
		{
			name:       "терминальный статус с телом",
			update:     `UPDATE email_outbox SET status = 'sent', locked_until = NULL WHERE id = $1`,
			constraint: "email_outbox_body_cleared_chk",
		},
		{
			name:       "sending без аренды",
			update:     `UPDATE email_outbox SET status = 'sending' WHERE id = $1`,
			constraint: "email_outbox_lock_chk",
		},
		{
			name:       "аренда у не-sending",
			update:     `UPDATE email_outbox SET locked_until = next_attempt_at WHERE id = $1`,
			constraint: "email_outbox_lock_chk",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			env := mustEnqueue(t, store, envelope())

			_, err := pool.Exec(context.Background(), tt.update, env.ID)

			require.Error(t, err)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "23514", pgErr.Code)
			assert.Equal(t, tt.constraint, pgErr.ConstraintName)
		})
	}
}
