package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier — общий знаменатель *pgxpool.Pool, *pgxpool.Conn и pgx.Tx: ровно то,
// чем пользуется адаптер хранилища. Ради него запросы адаптера не знают, идут
// они в пуле или в транзакции потребителя (образец — mailpg.Store).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
