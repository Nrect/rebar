package mailpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/mail"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, mail.ErrUnavailable) означает «письмо осталось в очереди».
//
// ТЕЛО ПИСЬМА НЕ ПОПАДАЕТ В ОШИБКУ. У pgconn.PgError на нарушении CHECK или
// уникальности в Detail лежит «Failing row contains (…)» — вся строка вместе
// с body_text и ссылкой с токеном. Поэтому *PgError не заворачивается в
// цепочку (иначе Detail достаётся через errors.As ниже по стеку), от него
// остаются SQLSTATE и Message.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%w: mailpg: %s: SQLSTATE %s: %s", mail.ErrUnavailable, op, pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("%w: mailpg: %s: %w", mail.ErrUnavailable, op, err)
}
