package shoppg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/postgres"
)

// Запросы к entitlement_grants. Схема — эталонная из entitlement/doc.go,
// скопированная в migrations как есть.
const (
	// Граница СТРОГАЯ (expires_at > $2): момент истечения уже закрыт — та же
	// граница, что у entitlement.Grant.Open. Читается префиксом первичного
	// ключа, поэтому отдельного индекса не нужно.
	//
	// granted_at читается обратно: без этого требование эталонной схемы
	// «время параметром, не DEFAULT now()» не проверялось бы ничем — адаптер
	// с DEFAULT now() прошёл бы весь контракт (entitlement/ports.go).
	selectGrantsSQL = `SELECT item_id, expires_at, granted_at FROM entitlement_grants
WHERE subject_id = $1 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY item_id`

	// ПОВТОРНАЯ ВЫДАЧА ПРОДЛЕВАЕТ СРОК, а не удваивает строку: повтор покупки
	// — штатное событие. Вместе со сроком обновляется и granted_at.
	// ON CONFLICT именно по имени ключа: «любое 23505 — продление» тихо съело
	// бы чужой конфликт.
	upsertGrantSQL = `INSERT INTO entitlement_grants
(subject_id, item_id, expires_at, granted_at, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT ON CONSTRAINT ` + uxGrants + ` DO UPDATE
SET expires_at = EXCLUDED.expires_at, granted_at = EXCLUDED.granted_at, source = EXCLUDED.source`

	deleteGrantSQL = `DELETE FROM entitlement_grants WHERE subject_id = $1 AND item_id = $2`
)

// sourcePurchase — значение колонки source эталонной схемы.
//
// КОНСТАНТА, А НЕ ПАРАМЕТР, и это вынужденно: колонка объявлена NOT NULL с
// комментарием «заказ, промо, ручная выдача», но у порта Store.Grant места
// под неё нет. Все выдачи этого примера приходят от оплаты, поэтому здесь
// значение честное; потребителю с промо и ручными выдачами колонка врала бы.
const sourcePurchase = "purchase"

// Entitlements — entitlement.Store поверх эталонной схемы.
//
// Адаптера у пакета нет и не будет в v0.1 (entitlement/doc.go, «Чего в пакете
// нет»): выдачи живут у потребителя, и у каждого они устроены по-своему.
//
// ЧАСОВ ЗДЕСЬ НЕТ. Момент приходит параметром Store.Grant, как и у Open, —
// адаптер, у которого есть собственное время, пишет его молча.
type Entitlements struct {
	db postgres.Querier
}

var _ entitlement.Store = (*Entitlements)(nil)

// NewEntitlements — адаптер на пуле.
func NewEntitlements(db *DB) *Entitlements { return &Entitlements{db: db.Pool} }

// WithTx — тот же адаптер в транзакции вызывающего. ЕДИНСТВЕННЫЙ путь, которым
// выдача ложится вместе с зачислением: без него человек оказывается
// заплатившим и не получившим.
func (s *Entitlements) WithTx(tx pgx.Tx) *Entitlements {
	if tx == nil {
		panic("shoppg.Entitlements.WithTx: nil tx")
	}
	return &Entitlements{db: tx}
}

// Open отдаёт выдачи, ОТКРЫТЫЕ В МОМЕНТ now. Выдач нет — пустой список и nil, а
// не ошибка: «ничего не куплено» это отказ по правилу, а не сбой.
func (s *Entitlements) Open(ctx context.Context, subjectID uuid.UUID,
	now time.Time,
) ([]entitlement.Grant, error) {
	rows, err := s.db.Query(ctx, selectGrantsSQL, subjectID, utc(now))
	if err != nil {
		return nil, storeError("чтение выдач", err)
	}
	defer rows.Close()

	var out []entitlement.Grant
	for rows.Next() {
		g, scanErr := scanGrant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("чтение выдач", err)
	}
	return out, nil
}

func scanGrant(row rowScanner) (entitlement.Grant, error) {
	var (
		g       entitlement.Grant
		expires *time.Time
	)
	if err := row.Scan(&g.ItemID, &expires, &g.GrantedAt); err != nil {
		return entitlement.Grant{}, storeError("чтение выдач", err)
	}
	g.GrantedAt = g.GrantedAt.UTC()
	if expires != nil {
		moment := expires.UTC()
		g.ExpiresAt = &moment
	}
	return g, nil
}

// Grant выдаёт право; повтор продлевает срок.
//
// МОМЕНТ ПРИХОДИТ ПАРАМЕТРОМ at; поле g.GrantedAt на записи игнорируется —
// так велит порт, и так его читает Open.
func (s *Entitlements) Grant(ctx context.Context, subjectID uuid.UUID,
	g entitlement.Grant, at time.Time,
) error {
	var expires *time.Time
	if g.ExpiresAt != nil {
		moment := g.ExpiresAt.UTC()
		expires = &moment
	}
	_, err := s.db.Exec(ctx, upsertGrantSQL, subjectID, g.ItemID, expires, utc(at), sourcePurchase)
	return storeError("выдача права", err)
}

// Revoke отзывает право. Отсутствие строки — не ошибка: отзыв идемпотентен,
// иначе повтор отмены платежа падал бы у потребителя.
func (s *Entitlements) Revoke(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	_, err := s.db.Exec(ctx, deleteGrantSQL, subjectID, itemID)
	return storeError("отзыв права", err)
}
