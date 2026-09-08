package auditpg

import (
	"fmt"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, audit.ErrUnavailable) означает «событие не записано».
//
// ГРАНИЦА ОШИБКИ — ОБЩАЯ, А НЕ СВОЯ КОПИЯ (ADR-0005, третья межмодульная
// зависимость). У pgconn.PgError в Detail лежит «Failing row contains (…)» —
// вся строка целиком, вместе с актором, целью и подробностями действия;
// postgres.Sanitize оставляет от неё SQLSTATE, Message и имя ограничения и не
// заворачивает *PgError в цепочку, поэтому Detail не достанется и через
// errors.As ниже по стеку. Своя копия этой границы была бы лишним шансом
// разойтись ровно там, где расхождение стоит утечки.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: auditpg: %s: %w", audit.ErrUnavailable, op, postgres.Sanitize(err))
}
