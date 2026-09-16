package idempg

import (
	"fmt"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/postgres"
)

// Сбои адаптера, отличимые от сбоя базы; класс приносит idem.ErrUnavailable:
// повтор получит запись.
var (
	// errPastLock — в WithTx вставка упёрлась в запись, легшую мимо блокировки
	// ключа, а эффект op уже в транзакции потребителя.
	errPastLock = fmt.Errorf("%w: idempg: record was written past the key lock", idem.ErrUnavailable)
	// errVanished — запись, в которую упёрлась вставка, не перечиталась.
	errVanished = fmt.Errorf("%w: idempg: conflicting record is gone", idem.ErrUnavailable)
)

// storeError — сбой Postgres в idem.ErrUnavailable.
//
// ГРАНИЦА ОШИБКИ ОБЩАЯ (ADR-0005): у pgconn.PgError в Detail лежит «Failing row
// contains (…)» — область, ключ и тело ответа. postgres.Sanitize оставляет
// SQLSTATE, Message и имя ограничения и *PgError в цепочку не кладёт.
func storeError(op string, err error) error {
	return fmt.Errorf("%w: idempg: %s: %w", idem.ErrUnavailable, op, postgres.Sanitize(err))
}
