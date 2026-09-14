package entitlementpg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/entitlement"
)

// Граница строгая, как у Grant.Open: момент истечения уже закрыт. Фильтр идёт
// префиксом первичного ключа; порядок побайтный, как у двойника, при любой
// сортировке базы потребителя.
const openSQL = `SELECT item_id, expires_at, granted_at FROM entitlement_grants
WHERE subject_id = $1 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY item_id COLLATE "C"`

// Open — выдачи, открытые в момент now; выдач нет — пустой срез и nil.
func (s *Store) Open(ctx context.Context, subjectID uuid.UUID, now time.Time) ([]entitlement.Grant, error) {
	rows, err := s.db.Query(ctx, openSQL, subjectID, now)
	if err != nil {
		return nil, storeError("open", err)
	}
	grants, err := pgx.CollectRows(rows, scanGrant)
	if err != nil {
		return nil, storeError("open", err)
	}
	return grants, nil
}

// scanGrant — строка в выдачу. GrantedAt читается из колонки: без обратного
// чтения DEFAULT now() в миграции прошёл бы весь контрактный набор.
func scanGrant(row pgx.CollectableRow) (entitlement.Grant, error) {
	var g entitlement.Grant
	if err := row.Scan(&g.ItemID, &g.ExpiresAt, &g.GrantedAt); err != nil {
		return entitlement.Grant{}, err
	}
	// pgx отдаёт timestamptz в местной зоне процесса; порт говорит о моментах.
	g.GrantedAt = g.GrantedAt.UTC()
	if g.ExpiresAt != nil {
		*g.ExpiresAt = g.ExpiresAt.UTC()
	}
	return g, nil
}

// ON CONFLICT ПО ИМЕНИ ПЕРВИЧНОГО КЛЮЧА, а не перехват 23505: повтор покупки —
// штатное событие, а ошибка Postgres перевела бы транзакцию потребителя в
// aborted вместе с оплатой.
//
// СРОК НИКОГДА НЕ СОКРАЩАЕТСЯ: бессрочная с любой стороны даёт бессрочную, из
// двух сроков остаётся поздний. Явный CASE, а не GREATEST: тот пропускает NULL
// и вернул бы срок там, где было бессрочно. Момент выдачи обновляется всегда.
const grantSQL = `INSERT INTO entitlement_grants AS cur (subject_id, item_id, expires_at, granted_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT ON CONSTRAINT ` + uxSubjectItem + ` DO UPDATE
SET expires_at = CASE
		WHEN cur.expires_at IS NULL OR EXCLUDED.expires_at IS NULL THEN NULL
		WHEN EXCLUDED.expires_at > cur.expires_at THEN EXCLUDED.expires_at
		ELSE cur.expires_at
	END,
	granted_at = EXCLUDED.granted_at`

// Grant — выдать предмет в момент at; g.GrantedAt на записи игнорируется.
func (s *Store) Grant(ctx context.Context, subjectID uuid.UUID, g entitlement.Grant, at time.Time) error {
	_, err := s.db.Exec(ctx, grantSQL, subjectID, g.ItemID, g.ExpiresAt, at)
	return storeError("grant", err)
}

const revokeSQL = `DELETE FROM entitlement_grants WHERE subject_id = $1 AND item_id = $2`

// Revoke — отозвать предмет. Ноль удалённых строк не ошибка: повтор отмены
// платежа не должен падать у потребителя.
func (s *Store) Revoke(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	_, err := s.db.Exec(ctx, revokeSQL, subjectID, itemID)
	return storeError("revoke", err)
}
