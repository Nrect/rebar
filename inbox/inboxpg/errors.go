package inboxpg

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/postgres"
)

var (
	// errNoHandler — у источника события нет обработчика: сборка разошлась с
	// источниками, которые ядро сверяет на старте.
	errNoHandler = errors.New("inboxpg: event source has no handler in the store")
	// errTxAborted — обработчик вернул nil в прерванной транзакции: он проглотил
	// ошибку своего запроса.
	errTxAborted = errors.New("inboxpg: handler returned nil in an aborted transaction")
)

// storeError — сбой Postgres и отменённый контекст в inbox.ErrUnavailable.
//
// ГРАНИЦА ОШИБКИ ОБЩАЯ (ADR-0005): у pgconn.PgError в Detail лежит «Failing row
// contains (…)» — тело события и его ключ. postgres.Sanitize оставляет SQLSTATE,
// Message и имя ограничения и *PgError в цепочку не кладёт.
func storeError(op string, err error) error {
	return fmt.Errorf("%w: inboxpg: %s: %w", inbox.ErrUnavailable, op, postgres.Sanitize(err))
}
