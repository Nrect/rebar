package mailpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/mail"
)

// prev_status возвращается вместе со строкой: только так видно, что строка
// пришла из sending с истёкшей арендой (Envelope.Reclaimed). Порядок задаёт
// внешний SELECT — у UPDATE … RETURNING порядка нет; next_attempt_at этот
// UPDATE не трогает, поэтому сортировка та же, что была до него.
const claimSQL = `WITH due AS (
	SELECT id, status AS prev_status
	FROM email_outbox
	WHERE (status = 'pending' AND next_attempt_at <= $1)
	   OR (status = 'sending' AND locked_until < $1)
	ORDER BY next_attempt_at, id
	LIMIT $3
	FOR UPDATE SKIP LOCKED
), claimed AS (
	UPDATE email_outbox SET
		status = 'sending',
		locked_until = $2,
		attempts = attempts + 1,
		updated_at = $1
	FROM due
	WHERE email_outbox.id = due.id
	RETURNING email_outbox.*, due.prev_status
)
SELECT ` + envelopeColumns + `, prev_status FROM claimed ORDER BY next_attempt_at, id`

// Claim забирает до limit строк к отправке под аренду до now+lease.
func (s *Store) Claim(
	ctx context.Context, now time.Time, lease time.Duration, limit int,
) ([]mail.Envelope, error) {
	rows, err := s.db.Query(ctx, claimSQL, now.UTC(), now.Add(lease).UTC(), limit)
	if err != nil {
		return nil, storeError("claim", err)
	}
	defer rows.Close()

	var claimed []mail.Envelope
	for rows.Next() {
		env, scanErr := scanClaimed(rows)
		if scanErr != nil {
			return nil, storeError("claim", scanErr)
		}
		claimed = append(claimed, env)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("claim", err)
	}
	return claimed, nil
}

func scanClaimed(rows pgx.Rows) (mail.Envelope, error) {
	var (
		env  mail.Envelope
		nt   nullableTimes
		prev mail.Status
	)
	if err := rows.Scan(append(envelopeDest(&env, &nt), &prev)...); err != nil {
		return mail.Envelope{}, err
	}
	toUTC(&env, nt)
	env.Reclaimed = prev == mail.StatusSending
	return env, nil
}
