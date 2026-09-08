package paymentpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/payment"
)

// ledgerColumns — порядок колонок для scanEntry; менять только вместе с ним.
const ledgerColumns = `id, intent_id, kind, amount_minor, currency, provider_event_id,
	reverses_entry_id, idempotency_key, actor_id, created_at`

// Порядок по возрастанию created_at — контракт порта. Дальше сортирует род
// записи, а не id: у зачисления и его возврата бывает одна и та же микросекунда
// (обе строки пишет одна транзакция), и по id возврат встал бы ПЕРЕД деньгами,
// которые он возвращает. Третий ключ — id: два возврата одного мгновения не
// должны меняться местами между чтениями.
const selectLedgerSQL = `SELECT ` + ledgerColumns + ` FROM payment_ledger
WHERE intent_id = $1 ORDER BY created_at, kind, id`

const selectEntryByKeySQL = `SELECT ` + ledgerColumns + ` FROM payment_ledger
WHERE intent_id = $1 AND idempotency_key = $2`

// ApplyRefund — компенсирующая запись в книгу: тот же контракт атомарности, что
// у ApplyEvent, и тот же порядок блокировок.
//
// ПОТОЛОК Σrefund ≤ Σcapture держат два независимых рубежа: проверка здесь, под
// блокировкой намерения, и триггер payment_ledger_refund_cap на самой книге.
// Первый снимает гонку двух частичных возвратов, второй ловит запись в обход
// сервиса — миграцию или правку руками в проде.
func (s *Store) ApplyRefund(ctx context.Context, req payment.ApplyRefundRequest,
) (payment.ApplyRefundResult, error) {
	if err := checkRefundShape(req); err != nil {
		return payment.ApplyRefundResult{}, err
	}
	return inTxResult(ctx, s, "apply refund",
		func(ctx context.Context, tx pgx.Tx) (payment.ApplyRefundResult, error) {
			return s.applyRefund(ctx, tx, req)
		})
}

func (s *Store) applyRefund(ctx context.Context, tx pgx.Tx, req payment.ApplyRefundRequest,
) (payment.ApplyRefundResult, error) {
	in, found, err := s.lockIntent(ctx, tx, req.IntentID)
	switch {
	case err != nil:
		return payment.ApplyRefundResult{}, err
	case !found:
		return payment.ApplyRefundResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}

	// UNIQUE (intent_id, idempotency_key), а не по ссылке на зачисление:
	// частичных возвратов на одно зачисление бывает несколько.
	existing, err := scanEntry(tx.QueryRow(ctx, selectEntryByKeySQL, req.IntentID,
		req.Refund.IdempotencyKey))
	switch {
	case err == nil:
		return payment.ApplyRefundResult{Outcome: payment.OutcomeDuplicateEvent, Entry: existing}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return payment.ApplyRefundResult{}, storeError("apply refund: duplicate probe", err)
	}

	entries, err := ledgerOf(ctx, tx, req.IntentID)
	if err != nil {
		return payment.ApplyRefundResult{}, err
	}
	net, err := payment.Net(entries, in.Currency)
	if err != nil {
		return payment.ApplyRefundResult{}, err
	}
	if req.Refund.AmountMinor > net.Minor() {
		return payment.ApplyRefundResult{Outcome: payment.OutcomeRefundTooLarge}, nil
	}

	if appendErr := appendLedger(ctx, tx, req.Refund); appendErr != nil {
		return payment.ApplyRefundResult{}, appendErr
	}
	if s.opts.Settler != nil {
		if in.Items, err = s.itemsOf(ctx, tx, in.ID); err != nil {
			return payment.ApplyRefundResult{}, err
		}
		if hookErr := s.opts.Settler.OnRefunded(ctx, tx, in, req.Refund); hookErr != nil {
			return payment.ApplyRefundResult{}, hookErr
		}
	}
	return payment.ApplyRefundResult{Outcome: payment.OutcomeApplied, Entry: req.Refund}, nil
}

// Ledger — записи намерения по возрастанию created_at.
func (s *Store) Ledger(ctx context.Context, intentID uuid.UUID) ([]payment.LedgerEntry, error) {
	return ledgerOf(ctx, s.db(), intentID)
}

func ledgerOf(ctx context.Context, q querier, intentID uuid.UUID) ([]payment.LedgerEntry, error) {
	rows, err := q.Query(ctx, selectLedgerSQL, intentID)
	if err != nil {
		return nil, storeError("ledger", err)
	}
	defer rows.Close()

	entries := make([]payment.LedgerEntry, 0, 2)
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, storeError("ledger", scanErr)
		}
		entries = append(entries, e)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("ledger", err)
	}
	return entries, nil
}

func scanEntry(s scanner) (payment.LedgerEntry, error) {
	var (
		e    payment.LedgerEntry
		kind string
	)
	err := s.Scan(&e.ID, &e.IntentID, &kind, &e.AmountMinor, &e.Currency, &e.ProviderEventID,
		&e.ReversesEntryID, &e.IdempotencyKey, &e.ActorID, &e.CreatedAt)
	if err != nil {
		return payment.LedgerEntry{}, err
	}
	e.Kind = payment.LedgerKind(kind)
	e.CreatedAt = e.CreatedAt.UTC()
	return e, nil
}

// checkRefundShape — форма запроса: запись принадлежит тому же намерению и
// гасит названное зачисление. Разойдись они — строка уехала бы мимо блокировки
// намерения, и потолок Σrefund ≤ capture считался бы по чужой книге.
func checkRefundShape(req payment.ApplyRefundRequest) error {
	if req.Refund.Kind != payment.LedgerRefund {
		return fmt.Errorf("%w: refund entry has kind %q", payment.ErrBadTransition, req.Refund.Kind)
	}
	if req.Refund.IntentID != req.IntentID {
		return fmt.Errorf("%w: refund entry belongs to intent %s, request to %s",
			payment.ErrBadTransition, req.Refund.IntentID, req.IntentID)
	}
	if req.Refund.ReversesEntryID == nil || *req.Refund.ReversesEntryID != req.CaptureEntryID {
		return fmt.Errorf("%w: refund entry does not reverse capture %s",
			payment.ErrBadTransition, req.CaptureEntryID)
	}
	return nil
}
