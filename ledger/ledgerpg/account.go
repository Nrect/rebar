package ledgerpg

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
)

// entryColumns — порядок колонок для scanEntry и вставки; менять только вместе.
const entryColumns = `id, book, account, seq, kind, amount_minor, balance_after_minor, reference, reverses_id,
reason, actor, idempotency_key, created_at, key_id, prev_hash, entry_hash`

const (
	entryByKeySQL = `SELECT ` + entryColumns + ` FROM ledger_entries
WHERE book = $1 AND account = $2 AND idempotency_key = $3`
	entryByIDSQL = `SELECT ` + entryColumns + ` FROM ledger_entries
WHERE id = $1 AND book = $2 AND account = $3`
	reversalOfSQL = `SELECT ` + entryColumns + ` FROM ledger_entries
WHERE reverses_id = $1 AND book = $2 AND account = $3`
	insertEntrySQL = `INSERT INTO ledger_entries (` + entryColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`
)

// accountTx — счёт под блокировкой Post (контракт ledger.AccountTx).
type accountTx struct {
	tx      pgx.Tx
	book    string
	account uuid.UUID
	// done — Post вернулся. В режиме WithTx транзакция потребителя ещё открыта,
	// и без флага утёкший счёт писал бы в неё мимо блокировки.
	done atomic.Bool
}

// EntryByKey — проба идемпотентности в пределах счёта.
func (a *accountTx) EntryByKey(ctx context.Context, key string) (ledger.Entry, bool, error) {
	return a.one(ctx, "entry by key", entryByKeySQL, a.book, a.account, key)
}

// EntryByID — запись этого счёта; запись чужого не находится.
func (a *accountTx) EntryByID(ctx context.Context, id uuid.UUID) (ledger.Entry, bool, error) {
	return a.one(ctx, "entry by id", entryByIDSQL, id, a.book, a.account)
}

// ReversalOf — отмена записи id.
func (a *accountTx) ReversalOf(ctx context.Context, id uuid.UUID) (ledger.Entry, bool, error) {
	return a.one(ctx, "reversal of", reversalOfSQL, id, a.book, a.account)
}

func (a *accountTx) one(ctx context.Context, op, query string, args ...any) (ledger.Entry, bool, error) {
	if a.done.Load() {
		return ledger.Entry{}, false, errTxDone
	}
	e, err := scanEntry(a.tx.QueryRow(ctx, query, args...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ledger.Entry{}, false, nil
	case err != nil:
		return ledger.Entry{}, false, storeError(op, err)
	}
	return e, true, nil
}

// Insert — вставка подписанной записи; отказы схемы — sentinel'ами контракта
// ledger.AccountTx.Insert. Чужой счёт виден до запроса: база не знает, какой
// счёт заблокирован.
func (a *accountTx) Insert(ctx context.Context, e ledger.Entry) error {
	if a.done.Load() {
		return errTxDone
	}
	if e.Book != a.book || e.Account != a.account {
		return fmt.Errorf("%w: ledgerpg: entry %s belongs to another account", ledger.ErrInvalidRequest, e.ID)
	}
	_, err := a.tx.Exec(ctx, insertEntrySQL,
		e.ID, e.Book, e.Account, e.Seq, e.Kind, e.AmountMinor, e.BalanceAfterMinor, e.Reference, e.ReversesID,
		e.Reason, e.Actor, e.IdempotencyKey, e.CreatedAt, int32(e.KeyID), notNull(e.PrevHash), notNull(e.EntryHash))
	return refusal("insert entry", err)
}

// notNull — пустая подпись уходит пустой, а не NULL: у pgx nil-срез — это NULL,
// и отказ пришёл бы нарушением NOT NULL без имени вместо ledger_entries_hash_chk.
func notNull(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// scanEntry — строка записи. Момент — в UTC: pgx отдаёт timestamptz в зоне
// соединения, а подпись сверяет момент.
func scanEntry(row pgx.Row) (ledger.Entry, error) {
	var (
		e     ledger.Entry
		keyID uint16
	)
	err := row.Scan(&e.ID, &e.Book, &e.Account, &e.Seq, &e.Kind, &e.AmountMinor, &e.BalanceAfterMinor,
		&e.Reference, &e.ReversesID, &e.Reason, &e.Actor, &e.IdempotencyKey, &e.CreatedAt, &keyID,
		&e.PrevHash, &e.EntryHash)
	if err != nil {
		return ledger.Entry{}, err
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.KeyID = secrets.KeyID(keyID)
	return e, nil
}
