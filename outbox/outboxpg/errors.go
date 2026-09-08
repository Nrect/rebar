package outboxpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/outbox"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, outbox.ErrUnavailable) означает «строка осталась в очереди».
//
// PAYLOAD НЕ ПОПАДАЕТ В ОШИБКУ. У pgconn.PgError на нарушении CHECK или
// уникальности в Detail лежит «Failing row contains (…)» — вся строка вместе
// с payload и заголовками. Поэтому *PgError не заворачивается в цепочку
// (иначе Detail достаётся через errors.As ниже по стеку), от него остаются
// SQLSTATE и Message — та же граница, что у postgres.Sanitize, но без
// межмодульной зависимости (ADR-0005, «Межмодульные зависимости»).
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%w: outboxpg: %s: SQLSTATE %s: %s",
			outbox.ErrUnavailable, op, pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("%w: outboxpg: %s: %w", outbox.ErrUnavailable, op, err)
}
