package authzpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/authz"
)

// ErrInvalidAssignment — назначение не прошло проверку до запроса: аноним,
// пустая роль, нулевое время выдачи. Отдельно от сбоя хранилища: чинить это
// в коде потребителя, а не в базе.
var ErrInvalidAssignment = errors.New("authzpg: assignment is invalid")

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, authz.ErrUnavailable) означает «решение не принято», то есть
// 503, а не 403.
//
// СТРОКА ТАБЛИЦЫ НЕ ПОПАДАЕТ В ОШИБКУ. У pgconn.PgError на нарушении CHECK в
// Detail лежит «Failing row contains (…)» — вся строка вместе с
// идентификатором субъекта и тем, кто выдал роль. Поэтому *PgError не
// заворачивается в цепочку (иначе Detail достаётся через errors.As ниже по
// стеку), от него остаются SQLSTATE и Message.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%w: authzpg: %s: SQLSTATE %s: %s", authz.ErrUnavailable, op, pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("%w: authzpg: %s: %w", authz.ErrUnavailable, op, err)
}
