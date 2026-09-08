package auditpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/audit"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, audit.ErrUnavailable) означает «событие не записано».
//
// ПОДРОБНОСТИ СОБЫТИЯ НЕ ПОПАДАЮТ В ОШИБКУ. У pgconn.PgError в Detail лежит
// «Failing row contains (…)» — вся строка целиком, вместе с адресом, логином
// и подробностями. Поэтому *PgError не заворачивается в цепочку (иначе Detail
// достался бы через errors.As ниже по стеку), от него остаются SQLSTATE и
// Message.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%w: auditpg: %s: SQLSTATE %s: %s", audit.ErrUnavailable, op, pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("%w: auditpg: %s: %w", audit.ErrUnavailable, op, err)
}
