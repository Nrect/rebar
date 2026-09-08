package authzpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/authz"
)

// Store — назначения ролей в таблице authz_role_assignments (schema.sql) и
// authz.RoleSource поверх них.
type Store struct {
	db  executor
	now func() time.Time
}

var _ authz.RoleSource = (*Store)(nil)

// executor — общий знаменатель *pgxpool.Pool и pgx.Tx: ровно то, чем
// пользуется адаптер. Ради него запросы не знают, идут они в пуле или в
// транзакции потребителя.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New паникует на nil-пуле: ошибка конфигурации падает на старте, а не на
// первой проверке прав.
func New(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("authzpg.New: nil pool")
	}
	return &Store{db: pool, now: func() time.Time { return time.Now().UTC() }}
}

// WithTx — тот же адаптер, но все запросы в транзакции потребителя: выдача
// роли ложится вместе с бизнес-фактом (приглашение сотрудника, покупка
// подписки), и падение между двумя транзакциями не оставляет роль без факта
// или факт без роли.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("authzpg.WithTx: nil tx")
	}
	return &Store{db: tx, now: s.now}
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
// Часы нужны одному вопросу — истёк ли срок назначения; в базу время уходит
// параметром, без DEFAULT now() и без now() в запросе (CONVENTIONS §9).
func (s *Store) SetClock(now func() time.Time) { s.now = now }
