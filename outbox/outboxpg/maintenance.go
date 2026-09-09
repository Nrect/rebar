package outboxpg

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/outbox"
)

// OldestDueAge считается по pending, ЧЕЙ СРОК УЖЕ НАСТУПИЛ: отложенная строка
// и строка под арендой возрастом не горят, иначе главный алерт пакета
// («очередь встала») светился бы от штатной работы. Unhandled — pending с
// типом вне реестра ЭТОГО воркера: при выкате их закономерно видит старый
// инстанс, поэтому алерт на метрику нужен с выдержкой (ADR-0002).
const statsSQL = `SELECT
	count(*) FILTER (WHERE status = 'pending'),
	count(*) FILTER (WHERE status = 'processing'),
	count(*) FILTER (WHERE status = 'failed'),
	count(*) FILTER (WHERE status = 'pending' AND NOT (kind = ANY($2))),
	min(available_at) FILTER (WHERE status = 'pending' AND available_at <= $1)
FROM outbox_messages`

// Stats — снимок очереди на момент now; known — типы, которые умеет воркер.
func (s *Store) Stats(ctx context.Context, now time.Time, known []outbox.Kind) (outbox.Stats, error) {
	var (
		stats  outbox.Stats
		oldest *time.Time
	)
	err := s.db.QueryRow(ctx, statsSQL, now.UTC(), kindStrings(known)).
		Scan(&stats.Pending, &stats.Processing, &stats.Failed, &stats.Unhandled, &oldest)
	if err != nil {
		return outbox.Stats{}, storeError("stats", err)
	}
	// Часы потребителя и строк могут разъехаться; отрицательный возраст гейджу не нужен.
	if oldest != nil && now.After(*oldest) {
		stats.OldestDueAge = now.Sub(oldest.UTC())
	}
	return stats, nil
}

const listFailedSQL = `SELECT ` + envelopeColumns + `
FROM outbox_messages WHERE status = 'failed' ORDER BY updated_at, id LIMIT $1`

// ListFailed — dead-letter для оператора, самые старые первыми. Непозитивный
// limit — пустая выборка без ошибки.
func (s *Store) ListFailed(ctx context.Context, limit int) ([]outbox.Envelope, error) {
	if limit <= 0 {
		return []outbox.Envelope{}, nil
	}
	rows, err := s.db.Query(ctx, listFailedSQL, limit)
	if err != nil {
		return nil, storeError("list failed", err)
	}
	defer rows.Close()

	failed := []outbox.Envelope{}
	for rows.Next() {
		env, scanErr := scanEnvelope(rows)
		if scanErr != nil {
			return nil, storeError("list failed", scanErr)
		}
		failed = append(failed, env)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("list failed", err)
	}
	return failed, nil
}

// last_error НЕ ОЧИЩАЕТСЯ: оператору видно, из-за чего строка попадала в
// dead-letter, — иначе redrive стирает единственный след разбора.
const redriveSQL = `UPDATE outbox_messages SET
	status = 'pending', attempts = 0, fail_reason = '', available_at = $2, updated_at = $2
WHERE id = $1 AND status = 'failed'`

// Redrive возвращает строку из failed в работу. false — строки нет либо она
// не в failed, и это не ошибка: повторный клик оператора не сбой.
func (s *Store) Redrive(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, redriveSQL, id, now.UTC())
	if err != nil {
		return false, storeError("redrive", err)
	}
	return tag.RowsAffected() > 0, nil
}

// failed НЕ ТРОГАЕТСЯ ни при каком before: молча исчезнувший dead-letter —
// это потерянное событие без следов (ADR-0002, «Безопасность», п. 7).
const purgeSQL = `DELETE FROM outbox_messages WHERE id IN (
	SELECT id FROM outbox_messages
	WHERE status IN ('done','expired') AND updated_at < $1
	ORDER BY updated_at, id
	LIMIT $2
)`

// Purge удаляет done и expired старше before, не больше limit за вызов.
func (s *Store) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil // отрицательный LIMIT Postgres отверг бы ошибкой
	}
	tag, err := s.db.Exec(ctx, purgeSQL, before.UTC(), limit)
	if err != nil {
		return 0, storeError("purge", err)
	}
	return int(tag.RowsAffected()), nil // не больше limit по построению запроса
}
