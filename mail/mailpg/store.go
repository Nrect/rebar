package mailpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/mail"
)

// Store — mail.Store поверх таблицы email_outbox (schema.sql).
type Store struct {
	db executor
}

var _ mail.Store = (*Store)(nil)

// executor — общий знаменатель *pgxpool.Pool и pgx.Tx: ровно то, чем
// пользуется адаптер. Ради него запросы не знают, идут они в пуле или в
// транзакции потребителя.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New паникует на nil-пуле: ошибка конфигурации падает на старте, а не на
// первом письме (как mail.NewService).
func New(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("mailpg.New: nil pool")
	}
	return &Store{db: pool}
}

// WithTx — тот же адаптер, но все запросы в транзакции потребителя: строка
// outbox ложится вместе с бизнес-фактом (ADR-0001, «В той же транзакции»).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("mailpg.WithTx: nil tx")
	}
	return &Store{db: tx}
}

// envelopeColumns — порядок колонок для envelopeDest; менять только вместе с ним.
const envelopeColumns = `id, kind, to_email, to_name, from_email, from_name, subject, body_text,
	body_html, headers, dedup_key, fingerprint, message_id, status, attempts, next_attempt_at,
	locked_until, last_error, fail_reason, transport, provider_message_id, not_after, created_at,
	updated_at, sent_at`

// scanner — общее у pgx.Row и pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// nullableTimes — колонки TIMESTAMPTZ NULL: сканируются отдельно, чтобы
// довести до UTC, не вписывая местную зону соединения в Envelope.
type nullableTimes struct {
	lockedUntil *time.Time
	notAfter    *time.Time
	sentAt      *time.Time
}

func envelopeDest(env *mail.Envelope, nt *nullableTimes) []any {
	return []any{
		&env.ID, &env.Kind, &env.To.Email, &env.To.Name, &env.From.Email, &env.From.Name,
		&env.Subject, &env.Text, &env.HTML, &env.Headers, &env.DedupKey, &env.Fingerprint,
		&env.MessageID, &env.Status, &env.Attempts, &env.NextAttemptAt, &nt.lockedUntil,
		&env.LastError, &env.FailReason, &env.Transport, &env.ProviderMessageID,
		&nt.notAfter, &env.CreatedAt, &env.UpdatedAt, &nt.sentAt,
	}
}

func scanEnvelope(s scanner) (mail.Envelope, error) {
	var (
		env mail.Envelope
		nt  nullableTimes
	)
	if err := s.Scan(envelopeDest(&env, &nt)...); err != nil {
		return mail.Envelope{}, err
	}
	toUTC(&env, nt)
	return env, nil
}

// toUTC — pgx отдаёт timestamptz в зоне соединения; порт говорит о моментах.
func toUTC(env *mail.Envelope, nt nullableTimes) {
	env.NextAttemptAt = env.NextAttemptAt.UTC()
	env.CreatedAt = env.CreatedAt.UTC()
	env.UpdatedAt = env.UpdatedAt.UTC()
	env.LockedUntil = utcPtr(nt.lockedUntil)
	env.NotAfter = utcPtr(nt.notAfter)
	env.SentAt = utcPtr(nt.sentAt)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}
