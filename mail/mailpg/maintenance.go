package mailpg

import (
	"context"
	"time"

	"github.com/nrect/rebar/mail"
)

const statsSQL = `SELECT
	count(*) FILTER (WHERE status IN ('pending','sending')),
	min(created_at) FILTER (WHERE status IN ('pending','sending')),
	count(*) FILTER (WHERE status = 'failed')
FROM email_outbox`

// Stats — снимок очереди; возраст считается от created_at, а не от
// next_attempt_at: гейдж должен показывать, сколько письмо ждёт на самом деле.
func (s *Store) Stats(ctx context.Context, now time.Time) (mail.Stats, error) {
	var (
		stats  mail.Stats
		oldest *time.Time
	)
	err := s.db.QueryRow(ctx, statsSQL).Scan(&stats.Pending, &oldest, &stats.Failed)
	if err != nil {
		return mail.Stats{}, storeError("stats", err)
	}
	// Часы потребителя и строк могут разъехаться; отрицательный возраст гейджу не нужен.
	if oldest != nil && now.After(*oldest) {
		stats.OldestPendingAge = now.Sub(*oldest)
	}
	return stats, nil
}

const purgeSQL = `DELETE FROM email_outbox WHERE id IN (
	SELECT id FROM email_outbox
	WHERE status IN ('sent','failed','expired','suppressed') AND updated_at < $1
	ORDER BY updated_at
	LIMIT $2
)`

// Purge удаляет терминальные строки старше before, не больше limit за вызов.
func (s *Store) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	tag, err := s.db.Exec(ctx, purgeSQL, before.UTC(), limit)
	if err != nil {
		return 0, storeError("purge", err)
	}
	return int(tag.RowsAffected()), nil // не больше limit по построению запроса
}
