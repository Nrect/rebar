package mailpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/mail"
)

// ON CONFLICT DO NOTHING, А НЕ ПЕРЕХВАТ 23505: в транзакции потребителя
// (WithTx) ошибка Postgres переводит всю транзакцию в aborted, и законный
// повтор письма — по контракту успех — ронял бы бизнес-факт вызывающего.
// Арбитр указан колонкой, поэтому нарушение любого другого UNIQUE
// (первичный ключ) остаётся ошибкой, а не превращается в дубль.
const insertSQL = `INSERT INTO email_outbox (` + envelopeColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
	$21, $22, $23, $24, $25)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING ` + envelopeColumns

const selectByDedupKeySQL = `SELECT ` + envelopeColumns + ` FROM email_outbox WHERE dedup_key = $1`

// Enqueue вставляет конверт как есть; на занятый dedup_key возвращает
// существующую строку — законность повтора решает домен по Fingerprint.
func (s *Store) Enqueue(ctx context.Context, env mail.Envelope) (mail.EnqueueResult, error) {
	headers := env.Headers
	if headers == nil {
		headers = map[string]string{} // nil-карта уехала бы как JSON null, а CHECK ждёт '{}'
	}
	inserted, err := scanEnvelope(s.db.QueryRow(ctx, insertSQL,
		env.ID, env.Kind, env.To.Email, env.To.Name, env.From.Email, env.From.Name,
		env.Subject, env.Text, env.HTML, headers, env.DedupKey, env.Fingerprint,
		env.MessageID, env.Status, env.Attempts, env.NextAttemptAt, env.LockedUntil,
		env.LastError, env.FailReason, env.Transport, env.ProviderMessageID,
		env.NotAfter, env.CreatedAt, env.UpdatedAt, env.SentAt,
	))
	switch {
	case err == nil:
		return mail.EnqueueResult{Outcome: mail.OutcomeInserted, Envelope: inserted}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return mail.EnqueueResult{}, storeError("enqueue", err)
	}

	existing, err := scanEnvelope(s.db.QueryRow(ctx, selectByDedupKeySQL, env.DedupKey))
	if err != nil {
		return mail.EnqueueResult{}, storeError("enqueue: duplicate row", err)
	}
	return mail.EnqueueResult{Outcome: mail.OutcomeDuplicate, Envelope: existing}, nil
}
