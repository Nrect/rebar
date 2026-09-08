package auditpg

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/audit"
)

// Sink — audit.Sink поверх таблицы audit_events (schema.sql).
type Sink struct {
	db executor
}

var _ audit.Sink = (*Sink)(nil)

// executor — общий знаменатель *pgxpool.Pool и pgx.Tx: ровно то, чем
// пользуется адаптер. Ради него запросы не знают, идут они в пуле или в
// транзакции потребителя.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New паникует на nil-пуле: ошибка сборки падает на старте, а не на первом
// событии.
func New(pool *pgxpool.Pool) *Sink {
	if pool == nil {
		panic("auditpg.New: nil pool")
	}
	return &Sink{db: pool}
}

// WithTx — тот же адаптер, но все запросы в транзакции потребителя: запись
// журнала ложится вместе с самим действием.
//
// БЕЗ НЕЁ ОДНО ИЗ ДВУХ ВСЕГДА ВРЁТ. Там, где действие стоит денег, «действие
// без записи» и «запись без действия» одинаково недопустимы, а падение между
// двумя транзакциями даёт то одно, то другое. Контракт держит тест
// TestSink_WithTx_IsAtomic.
func (s *Sink) WithTx(tx pgx.Tx) *Sink {
	if tx == nil {
		panic("auditpg.WithTx: nil tx")
	}
	return &Sink{db: tx}
}

// eventColumns — порядок колонок вставки; менять только вместе с insertSQL.
const eventColumns = `id, occurred_at, action, outcome, actor_kind, actor_id, actor_name,
	target_type, target_id, request_id, ip, details`

// INSERT И БОЛЬШЕ НИЧЕГО. Ни UPDATE, ни DELETE в адаптере нет: журнал —
// история, а не состояние (audit/doc.go, п. 5). Держит
// TestAdapter_HasNoUpdateOrDelete плюс триггер схемы.
const insertSQL = `INSERT INTO audit_events (` + eventColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// Write вставляет событие как есть: оно уже проверено и усечено ядром.
func (s *Sink) Write(ctx context.Context, ev audit.Event) error {
	details := ev.Details
	if details == nil {
		details = map[string]string{} // nil-карта уехала бы как JSON null, а колонка ждёт '{}'
	}
	_, err := s.db.Exec(ctx, insertSQL,
		ev.ID, ev.At, ev.Action, ev.Outcome, ev.Actor.Kind, ev.Actor.ID, ev.Actor.Name,
		ev.Target.Type, ev.Target.ID, ev.RequestID, ev.IP, details,
	)
	return storeError("write", err)
}
