package authzpg

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/postgres"
)

// ErrInvalidAssignment — назначение не прошло проверку до запроса: аноним,
// пустая роль, нулевое время выдачи. Отдельно от сбоя хранилища: чинить это
// в коде потребителя, а не в базе.
var ErrInvalidAssignment = errors.New("authzpg: assignment is invalid")

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, authz.ErrUnavailable) означает «решение не принято», то есть
// 503, а не 403.
//
// ГРАНИЦА СОДЕРЖИМОГО СТРОКИ — postgres.Sanitize, ОДНА НА ВЕСЬ ТУЛКИТ. В
// Detail ошибки Postgres лежит «Failing row contains (…)» — вся строка вместе
// с идентификатором субъекта и тем, кто выдал роль. Своей копии этой проверки
// здесь нет намеренно: пять копий в пяти адаптерах — пять шансов разойтись
// ровно там, где расхождение стоит утечки (ADR-0005, «Межмодульные
// зависимости»). Sanitize оставляет SQLSTATE, Message и имя ограничения и не
// заворачивает *pgconn.PgError в цепочку, поэтому Detail не достаётся и через
// errors.As ниже по стеку.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: authzpg: %s: %w", authz.ErrUnavailable, op, postgres.Sanitize(err))
}
