package outboxpg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/outbox"
)

// Store — outbox.Store поверх таблицы outbox_messages (schema.sql).
type Store struct {
	db executor
}

var _ outbox.Store = (*Store)(nil)

// executor — общий знаменатель *pgxpool.Pool и pgx.Tx: ровно то, чем
// пользуется адаптер. Ради него запросы не знают, идут они в пуле или в
// транзакции потребителя.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New паникует на nil-пуле: ошибка конфигурации падает на старте, а не на
// первом событии (как outbox.NewWorker).
func New(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("outboxpg.New: nil pool")
	}
	return &Store{db: pool}
}

// WithTx — тот же адаптер, но все запросы в транзакции потребителя: строка
// очереди ложится вместе с бизнес-фактом. Другого пути вставки у пакета нет
// (ADR-0002, «Enqueue только в транзакции»).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("outboxpg.WithTx: nil tx")
	}
	return &Store{db: tx}
}

// envelopeColumns — порядок колонок для envelopeDest; менять только вместе с ним.
const envelopeColumns = `id, kind, payload, headers, aggregate_type, aggregate_id, schema_version,
	dedup_key, fingerprint, status, attempts, available_at, not_after, occurred_at, claim_token,
	locked_until, last_error, fail_reason, created_at, updated_at, done_at`

// scanner — общее у pgx.Row и pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// nullables — колонки, допускающие NULL: сканируются отдельно, чтобы довести
// время до UTC, не вписывая местную зону соединения в Envelope.
type nullables struct {
	notAfter    *time.Time
	claimToken  *uuid.UUID
	lockedUntil *time.Time
	doneAt      *time.Time
}

func envelopeDest(env *outbox.Envelope, n *nullables) []any {
	return []any{
		&env.ID, &env.Kind, &env.Payload, &env.Headers, &env.AggregateType, &env.AggregateID,
		&env.SchemaVersion, &env.DedupKey, &env.Fingerprint, &env.Status, &env.Attempts,
		&env.AvailableAt, &n.notAfter, &env.OccurredAt, &n.claimToken, &n.lockedUntil,
		&env.LastError, &env.FailReason, &env.CreatedAt, &env.UpdatedAt, &n.doneAt,
	}
}

func scanEnvelope(s scanner) (outbox.Envelope, error) {
	var (
		env outbox.Envelope
		n   nullables
	)
	if err := s.Scan(envelopeDest(&env, &n)...); err != nil {
		return outbox.Envelope{}, err
	}
	toUTC(&env, n)
	return env, nil
}

// toUTC — pgx отдаёт timestamptz в зоне соединения; порт говорит о моментах.
func toUTC(env *outbox.Envelope, n nullables) {
	env.AvailableAt = env.AvailableAt.UTC()
	env.OccurredAt = env.OccurredAt.UTC()
	env.CreatedAt = env.CreatedAt.UTC()
	env.UpdatedAt = env.UpdatedAt.UTC()
	env.NotAfter = utcPtr(n.notAfter)
	env.LockedUntil = utcPtr(n.lockedUntil)
	env.DoneAt = utcPtr(n.doneAt)
	env.ClaimToken = n.claimToken
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// headersOf — nil-карта уехала бы как JSON null, а колонка объявлена NOT NULL
// со значением '{}'.
func headersOf(env outbox.Envelope) map[string]string {
	if env.Headers == nil {
		return map[string]string{}
	}
	return env.Headers
}

// kindStrings — []Kind в массив для `kind = ANY($n)`. Пустой список остаётся
// пустым массивом, а не NULL: с NULL сравнение дало бы NULL, и фильтр
// «известные типы» молча пропустил бы всё.
func kindStrings(kinds []outbox.Kind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}
