package outboxpg

import (
	"fmt"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, outbox.ErrUnavailable) означает «строка осталась в очереди».
//
// PAYLOAD НЕ ПОПАДАЕТ В ОШИБКУ. Границу держит postgres.Sanitize: у
// pgconn.PgError в Detail лежит «Failing row contains (…)» — вся строка вместе
// с payload и заголовками, поэтому *PgError не заворачивается в цепочку
// (иначе Detail достаётся через errors.As ниже по стеку), и от ошибки
// Postgres остаются SQLSTATE, Message и имя ограничения.
//
// Граница общая, а не скопированная: пять адаптеров с пятью её копиями — это
// пять шансов разойтись ровно там, где расхождение стоит утечки содержимого
// строки (ADR-0005, «Межмодульные зависимости»). Классификаторы модуля
// (postgres.IsUniqueViolation по имени) работают и после обёртки.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: outboxpg: %s: %w", outbox.ErrUnavailable, op, postgres.Sanitize(err))
}
