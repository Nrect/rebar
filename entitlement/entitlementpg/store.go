package entitlementpg

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/postgres"
)

// Store — выдачи в таблице entitlement_grants (Migrations) и entitlement.Store
// поверх неё. Часов у адаптера нет: всё время приходит параметрами порта.
type Store struct {
	db postgres.Querier
}

var _ entitlement.Store = (*Store)(nil)

// New паникует на nil-пуле: ошибка проводки падает на старте, а не на первой
// проверке доступа.
func New(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("entitlementpg.New: nil pool")
	}
	return &Store{db: pool}
}

// WithTx — тот же адаптер в транзакции потребителя: выдача ложится вместе с
// оплатой, и падение между двумя транзакциями не оставляет оплату без доступа
// (CONVENTIONS §10).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("entitlementpg.WithTx: nil tx")
	}
	return &Store{db: tx}
}
