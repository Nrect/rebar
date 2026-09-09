package mailpg

import (
	"fmt"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, mail.ErrUnavailable) означает «письмо осталось в очереди».
//
// ГРАНИЦА ОШИБКИ — ОБЩАЯ, А НЕ СВОЯ КОПИЯ (ADR-0005, третья межмодульная
// зависимость). У pgconn.PgError в Detail лежит «Failing row contains (…)» —
// вся строка целиком, вместе с телом письма и ссылкой с токеном;
// postgres.Sanitize оставляет от неё SQLSTATE, Message и имя ограничения и не
// заворачивает *PgError в цепочку, поэтому Detail не достанется и через
// errors.As ниже по стеку. Своя копия вдобавок теряла имя ограничения, и
// потребитель mail не мог отличить один конфликт от другого так же, как у
// соседних адаптеров.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: mailpg: %s: %w", mail.ErrUnavailable, op, postgres.Sanitize(err))
}
