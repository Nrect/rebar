package ledgerpg

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// Ошибки адаптера, отличимые от доменных; класс приносит обёрнутая sentinel.
var (
	// errTxDone — счёт тронут после того, как Post вернулся.
	errTxDone = fmt.Errorf("%w: ledgerpg: locked account used after Post returned", ledger.ErrUnavailable)
	// errUnknownBook — книга не передана в New либо её нет в ledger_books: дефект
	// сборки, а не сбой.
	errUnknownBook = fmt.Errorf("%w: ledgerpg: book is not registered", ledger.ErrInvalidRequest)
)

// refusals — отказ схемы по имени ограничения, а не по SQLSTATE: у 23514 и 23503
// здесь по нескольку исходов (контракт ledger.AccountTx.Insert). Разрыв номера и
// цепи приходит 40001 и остаётся сбоем: транзакцию повторяют.
var refusals = map[string]error{
	ckEntriesFloor:              ledger.ErrInsufficientFunds,
	fkEntriesKind:               ledger.ErrUnknownKind,
	ckEntriesReversalTarget:     ledger.ErrEntryNotFound,
	ckEntriesReversalOfReversal: ledger.ErrNotReversible,
	uxEntriesReversal:           ledger.ErrAlreadyReversed,
	fkAccountsBook:              errUnknownBook,

	fkEntriesAccount:        ledger.ErrInvalidRequest,
	ckEntriesAmount:         ledger.ErrInvalidRequest,
	ckEntriesSign:           ledger.ErrInvalidRequest,
	ckEntriesRequired:       ledger.ErrInvalidRequest,
	ckEntriesKey:            ledger.ErrInvalidRequest,
	ckEntriesKeyID:          ledger.ErrInvalidRequest,
	ckEntriesHash:           ledger.ErrInvalidRequest,
	ckEntriesReversal:       ledger.ErrInvalidRequest,
	ckEntriesReversalAmount: ledger.ErrInvalidRequest,
	ckEntriesBalanceRange:   ledger.ErrInvalidRequest,

	ckEntriesChain:     ledger.ErrUnavailable,
	ckEntriesHeadMoved: ledger.ErrUnavailable,
	ckEntriesTaken:     ledger.ErrUnavailable,
}

// storeError — сбой Postgres в ledger.ErrUnavailable.
//
// ГРАНИЦА ОШИБКИ ОБЩАЯ (ADR-0005): у pgconn.PgError в Detail лежит «Failing row
// contains (…)» — причина оператора, ключ клиента, суммы. postgres.Sanitize
// оставляет SQLSTATE, Message и имя ограничения и *PgError в цепочку не кладёт.
func storeError(op string, err error) error {
	return fmt.Errorf("%w: ledgerpg: %s: %w", ledger.ErrUnavailable, op, postgres.Sanitize(err))
}

// refusal — отказ схемы своей sentinel, остальное — сбой. nil остаётся nil.
func refusal(op string, err error) error {
	if err == nil {
		return nil
	}
	clean := postgres.Sanitize(err)
	var pgErr *postgres.Error
	if errors.As(clean, &pgErr) {
		if sentinel, ok := refusals[pgErr.Constraint]; ok {
			return fmt.Errorf("%w: ledgerpg: %s: %w", sentinel, op, clean)
		}
	}
	return storeError(op, err)
}
