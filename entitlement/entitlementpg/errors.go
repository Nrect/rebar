package entitlementpg

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта.
//
// ГРАНИЦА СОДЕРЖИМОГО СТРОКИ — postgres.Sanitize, ОДНА НА ТУЛКИТ. В Detail
// лежит «Failing row contains (…)» с идентификатором субъекта; Sanitize
// оставляет SQLSTATE, Message и имя ограничения и не заворачивает
// *pgconn.PgError в цепочку, поэтому Detail не достаётся и через errors.As.
// Своей копии границы нет намеренно (ADR-0005, «Межмодульные зависимости»).
//
// ПОТОЛОК ПРЕДМЕТА — entitlement.ErrInvalidGrant, а не ErrUnavailable: база
// ответила определённо, и повтор не поможет. Разбор ПО ИМЕНИ, а не по 23514:
// свой CHECK потребителя в той же таблице — чужой отказ, а не негодный предмет.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	clean := postgres.Sanitize(err)
	var class error = entitlement.ErrUnavailable
	var sanitized *postgres.Error
	if errors.As(clean, &sanitized) && sanitized.Constraint == ckItemID {
		class = entitlement.ErrInvalidGrant
	}
	return fmt.Errorf("%w: entitlementpg: %s: %w", class, op, clean)
}
