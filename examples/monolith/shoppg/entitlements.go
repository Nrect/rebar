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
	selectGrantsSQL = `SELECT item_id, expires_at FROM entitlement_grants
WHERE subject_id = $1 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY item_id`

	// ПОВТОРНАЯ ВЫДАЧА ПРОДЛЕВАЕТ СРОК, а не удваивает строку: повтор покупки
	// — штатное событие. ON CONFLICT именно по имени ключа: «любое 23505 —
	// продление» тихо съело бы чужой конфликт.
	upsertGrantSQL = `INSERT INTO entitlement_grants
(subject_id, item_id, expires_at, granted_at, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT ON CONSTRAINT ` + uxGrants + ` DO UPDATE
SET expires_at = EXCLUDED.expires_at, granted_at = EXCLUDED.granted_at, source = EXCLUDED.source`

	deleteGrantSQL = `DELETE FROM entitlement_grants WHERE subject_id = $1 AND item_id = $2`
)

// SourcePurchase — основание выдачи по оплате. Колонка source нужна, чтобы
// «кто это открыл» имело ответ: журнал выдач пакет не ведёт (entitlement/doc.go).
const SourcePurchase = "purchase"

// Entitlements — entitlement.Store поверх эталонной схемы.
//
// Адаптера у пакета нет и не будет в v0.1 (entitlement/doc.go, «Чего в пакете
// нет»): выдачи живут у потребителя, и у каждого они устроены по-своему.
type Entitlements struct {
	db  postgres.Querier
	now func() time.Time
}

var _ entitlement.Store = (*Entitlements)(nil)

// NewEntitlements — адаптер на пуле.
func NewEntitlements(db *DB) *Entitlements {
	return &Entitlements{db: db.Pool, now: time.Now}
}

// SetClock подменяет часы; зовётся до начала обслуживания.
//
// Часы адаптеру нужны только из-за Grant: у порта нет параметра времени, а
// колонка granted_at без DEFAULT now() его требует — doc.go, «Что не сошлось».
func (s *Entitlements) SetClock(now func() time.Time) {
	if now == nil {
		panic("shoppg.Entitlements.SetClock: now must not be nil")
	}
	s.now = now
}

// WithTx — тот же адаптер в транзакции вызывающего. ЕДИНСТВЕННЫЙ путь, которым
// выдача ложится вместе с зачислением: без него человек оказывается
// заплатившим и не получившим.
func (s *Entitlements) WithTx(tx pgx.Tx) *Entitlements {
	if tx == nil {
		panic("shoppg.Entitlements.WithTx: nil tx")
	}
	return &Entitlements{db: tx, now: s.now}
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
		var g entitlement.Grant
		var expires *time.Time
		if err := rows.Scan(&g.ItemID, &expires); err != nil {
			return nil, storeError("чтение выдач", err)
		}
		if expires != nil {
			moment := expires.UTC()
			g.ExpiresAt = &moment
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("чтение выдач", err)
	}
	return out, nil
}

// Grant выдаёт право; повтор продлевает срок.
//
// ВРЕМЯ БЕРЁТСЯ ИЗ ЧАСОВ АДАПТЕРА, потому что у порта его нет: единственное
// место в примере, где момент не приходит параметром. Транзакционный путь
// (хук зачисления) зовёт GrantAt и передаёт момент явно.
func (s *Entitlements) Grant(ctx context.Context, subjectID uuid.UUID, g entitlement.Grant) error {
	return s.GrantAt(ctx, subjectID, g, s.now(), SourcePurchase)
}

// GrantAt — та же выдача, но время и основание приходят параметром. Порт
// entitlement.Store времени не передаёт, а колонка granted_at без DEFAULT
// now() его требует: тест на управляемых часах иначе проверяет одно, а база
// пишет другое.
func (s *Entitlements) GrantAt(ctx context.Context, subjectID uuid.UUID, g entitlement.Grant,
	at time.Time, source string,
) error {
	var expires *time.Time
	if g.ExpiresAt != nil {
		moment := g.ExpiresAt.UTC()
		expires = &moment
	}
	_, err := s.db.Exec(ctx, upsertGrantSQL, subjectID, g.ItemID, expires, utc(at), source)
	return storeError("выдача права", err)
}

// Revoke отзывает право. Отсутствие строки — не ошибка: отзыв идемпотентен,
// иначе повтор отмены платежа падал бы у потребителя.
func (s *Entitlements) Revoke(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	_, err := s.db.Exec(ctx, deleteGrantSQL, subjectID, itemID)
	return storeError("отзыв права", err)
}
