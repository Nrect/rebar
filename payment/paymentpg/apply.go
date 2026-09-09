package paymentpg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/postgres"
)

// Блокировка намерения идёт ПЕРВОЙ и до вставки строки дедупа: внешний ключ
// события берёт на родительской строке свою блокировку, и две доставки про одно
// намерение, зашедшие в обратном порядке, дедлочили бы друг друга.
const lockIntentSQL = `SELECT ` + intentColumns + ` FROM payment_intents WHERE id = $1 FOR UPDATE`

// Строка дедупа: она же арбитр «кто занял — тот и делает работу». Повтор не
// пишет ничего, кроме счётчика доставок, — растущий счётчик означает, что наш
// ответ до провайдера не доезжает.
//
// deliveries == 1 в ответе значит «вставили мы»: у новой строки счётчик равен
// единице по умолчанию, а у повтора он уже увеличен и не меньше двух.
const insertEventSQL = `INSERT INTO payment_events (provider, provider_event_id, intent_id, kind,
	amount_minor, currency, provider_payment_id, occurred_at, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT ON CONSTRAINT ` + uxEventsDedup + `
DO UPDATE SET deliveries = payment_events.deliveries + 1
RETURNING deliveries`

const applyStatusSQL = `UPDATE payment_intents SET status = $2, updated_at = $3, settled_at = $4
WHERE id = $1 AND status = ANY($5)
RETURNING ` + intentColumns

const insertLedgerSQL = `INSERT INTO payment_ledger (id, intent_id, kind, amount_minor, currency,
	provider_event_id, reverses_entry_id, idempotency_key, actor_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// ApplyEvent — единственная операция зачисления: блокировка намерения, строка
// дедупа, предикат домена, статус, книга и хук потребителя одной транзакцией.
//
// Атомарность контракта доказывается TestStore_ApplyEvent_IsAtomic: ошибка хука
// обязана откатить и статус, и книгу, и строку дедупа, — иначе повтор вебхука
// увидел бы дубль, не применил бы ничего, а провайдер получил бы 200 на
// неучтённую оплату.
func (s *Store) ApplyEvent(ctx context.Context, req payment.ApplyEventRequest,
) (payment.ApplyEventResult, error) {
	if err := checkEventShape(req); err != nil {
		return payment.ApplyEventResult{}, err
	}
	return inTxResult(ctx, s, "apply event",
		func(ctx context.Context, tx pgx.Tx) (payment.ApplyEventResult, error) {
			return s.applyEvent(ctx, tx, req)
		})
}

func (s *Store) applyEvent(ctx context.Context, tx pgx.Tx, req payment.ApplyEventRequest,
) (payment.ApplyEventResult, error) {
	in, found, err := s.lockIntent(ctx, tx, req.IntentID)
	if err != nil {
		return payment.ApplyEventResult{}, err
	}
	if found {
		if in.Items, err = s.itemsOf(ctx, tx, in.ID); err != nil {
			return payment.ApplyEventResult{}, err
		}
	}

	first, err := recordEvent(ctx, tx, req.Event, intentRef(in, found), req.Now)
	switch {
	case err != nil:
		return payment.ApplyEventResult{}, err
	case !first:
		return payment.ApplyEventResult{Outcome: payment.OutcomeDuplicateEvent, Intent: in}, nil
	case !found:
		// Орфан: строка события записана, применять не к чему.
		return payment.ApplyEventResult{Outcome: payment.OutcomeUnknownIntent}, nil
	case req.To == "":
		return payment.ApplyEventResult{Outcome: payment.OutcomeIgnored, Intent: in}, nil
	case len(req.ExpectFrom) == 0:
		// Ошибка программиста, а не событие: транзакция откатывается целиком,
		// чтобы исправленный домен смог применить это же событие.
		return payment.ApplyEventResult{}, fmt.Errorf(
			"%w: apply event to %s requires a non-empty ExpectFrom", payment.ErrBadTransition, req.To)
	}
	if outcome, blocked := checkApplyPredicate(in, req); blocked {
		return payment.ApplyEventResult{Outcome: outcome, Intent: in}, nil
	}
	return s.settle(ctx, tx, req, in)
}

// settle — статус, книга и хук: три шага, после которых остаётся только commit.
func (s *Store) settle(ctx context.Context, tx pgx.Tx, req payment.ApplyEventRequest,
	in payment.Intent,
) (payment.ApplyEventResult, error) {
	updated, err := scanIntent(tx.QueryRow(ctx, applyStatusSQL, req.IntentID, string(req.To),
		req.Now, settledMoment(req, in), statusStrings(req.ExpectFrom)))
	if err != nil {
		return payment.ApplyEventResult{}, storeError("apply event: status", err)
	}
	updated.Items = in.Items

	if settleErr := s.appendAndSettle(ctx, tx, req.Ledger, updated); settleErr != nil {
		return payment.ApplyEventResult{}, settleErr
	}
	return payment.ApplyEventResult{Outcome: payment.OutcomeApplied, Intent: updated}, nil
}

// appendAndSettle — книга и хук потребителя, в этом порядке. Хук зовётся после
// книги и до commit: его ошибка откатывает всё, включая строку дедупа события.
func (s *Store) appendAndSettle(ctx context.Context, tx pgx.Tx, entry *payment.LedgerEntry,
	in payment.Intent,
) error {
	if entry == nil {
		return nil
	}
	if err := appendLedger(ctx, tx, *entry); err != nil {
		return err
	}
	if s.opts.Settler == nil {
		return nil
	}
	return s.opts.Settler.OnSettled(ctx, tx, in, *entry)
}

// lockIntent — строка намерения под FOR UPDATE. uuid.Nil означает орфана:
// блокировать нечего, и шаг пропускается.
func (s *Store) lockIntent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (payment.Intent, bool, error) {
	if id == uuid.Nil {
		return payment.Intent{}, false, nil
	}
	in, err := scanIntent(tx.QueryRow(ctx, lockIntentSQL, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return payment.Intent{}, false, nil
	case errors.Is(err, payment.ErrBadStatus):
		return payment.Intent{}, false, err
	case err != nil:
		return payment.Intent{}, false, storeError("lock intent", err)
	}
	return in, true, nil
}

// recordEvent — строка приёма события; first == false означает повтор.
//
// Орфану пишется intent_id IS NULL: внешнего ключа для чужого намерения нет, а
// потерянный орфан — это невидимая утечка ключа подписи либо вебхук со стенда,
// прилетевший в прод.
func recordEvent(ctx context.Context, tx pgx.Tx, ev payment.Event, intentID *uuid.UUID,
	now time.Time,
) (first bool, err error) {
	var deliveries int
	err = tx.QueryRow(ctx, insertEventSQL, string(ev.Provider), ev.ProviderEventID, intentID,
		string(ev.Type), ev.AmountMinor, ev.Currency, ev.ProviderPaymentID,
		ev.OccurredAt, now).Scan(&deliveries)
	if err != nil {
		return false, storeError("record event", err)
	}
	return deliveries == 1, nil
}

// intentRef — ссылка на намерение для строки события: только на строку, которую
// мы под блокировкой ВИДЕЛИ. Взять id из тела события значило бы упереться во
// внешний ключ ровно тогда, когда событие и так некуда применить.
func intentRef(in payment.Intent, found bool) *uuid.UUID {
	if !found {
		return nil
	}
	return nilIfEmpty(in.ID)
}

// appendLedger — запись в книгу.
//
// Отбой рубежей базы разбирается ПО ИМЕНИ и уезжает наружу громко (503 и
// разбор), а не исходом: и занятый индекс зачисления, и триггер потолка тут
// означают, что книга разошлась с тем, что адаптер видел под блокировкой, —
// то есть в неё писали мимо сервиса. Тихий «дубль» на это ответил бы 200 и
// оставил бы деньги неучтёнными навсегда.
func appendLedger(ctx context.Context, tx pgx.Tx, e payment.LedgerEntry) error {
	_, err := tx.Exec(ctx, insertLedgerSQL, e.ID, e.IntentID, string(e.Kind), e.AmountMinor,
		e.Currency, e.ProviderEventID, e.ReversesEntryID, e.IdempotencyKey, e.ActorID, e.CreatedAt)
	switch {
	case postgres.IsUniqueViolation(err, uxLedgerCapture):
		return fmt.Errorf("%w: paymentpg: append ledger: %s: зачисление на это намерение уже записано",
			payment.ErrUnavailable, uxLedgerCapture)
	case postgres.IsUniqueViolation(err, uxLedgerKey):
		return fmt.Errorf("%w: paymentpg: append ledger: %s: запись с этим ключом уже записана",
			payment.ErrUnavailable, uxLedgerKey)
	case raisedBy(err, ckLedgerRefundCap):
		return fmt.Errorf("%w: paymentpg: append ledger: %s: сумма возвратов превысила бы зачисление",
			payment.ErrUnavailable, ckLedgerRefundCap)
	case raisedBy(err, ckLedgerRefundCurrency):
		return fmt.Errorf("%w: paymentpg: append ledger: %s: возврат в чужой валюте",
			payment.ErrUnavailable, ckLedgerRefundCurrency)
	}
	return storeError("append ledger", err)
}

// settledMoment — момент зачисления: время события, зажатое в
// [intent.CreatedAt, req.Now].
//
// Берётся из события, потому что оно может доехать через час после списания, и
// выручка «за январь» уехала бы в февраль. Зажимается, потому что время события
// приходит из внешнего мира и не проверено ничем, а колонка, по которой режут
// выручку, после зачисления неисправима: книга append-only.
func settledMoment(req payment.ApplyEventRequest, in payment.Intent) any {
	if req.To != payment.StatusSucceeded {
		return nil
	}
	at := req.Event.OccurredAt.UTC()
	if at.Before(in.CreatedAt) {
		at = in.CreatedAt
	}
	if at.After(req.Now) {
		at = req.Now
	}
	return at
}

// checkEventShape — форма запроса, которую адаптер обязан требовать ДО первой
// записи: книга непуста тогда и только тогда, когда цель — succeeded, и запись
// принадлежит тому же намерению.
//
// Каждое из этих расхождений неисправимо, потому что книга append-only.
// Зачисление без книги — это succeeded_no_capture: деньги получены, денежной
// записи нет. Книга без зачисления — движение денег на переходе, который
// деньгами не является. Запись, уехавшая на ЧУЖОЕ намерение, занимает его
// уникальный индекс зачисления, и собственная законная оплата того намерения не
// запишется уже никогда.
func checkEventShape(req payment.ApplyEventRequest) error {
	if req.Ledger == nil {
		if req.To == payment.StatusSucceeded {
			return fmt.Errorf("%w: settling %s requires a ledger entry",
				payment.ErrBadTransition, req.IntentID)
		}
		return nil
	}
	if req.To != payment.StatusSucceeded {
		return fmt.Errorf("%w: a ledger entry belongs to a settlement, not to %q",
			payment.ErrBadTransition, req.To)
	}
	if req.Ledger.Kind != payment.LedgerCapture {
		return fmt.Errorf("%w: settlement writes a capture, got %q",
			payment.ErrBadTransition, req.Ledger.Kind)
	}
	if req.Ledger.IntentID != req.IntentID {
		return fmt.Errorf("%w: ledger entry belongs to intent %s, event to %s",
			payment.ErrBadTransition, req.Ledger.IntentID, req.IntentID)
	}
	return nil
}

// checkApplyPredicate — предикат домена в порядке контракта порта: статус,
// затем деньги, и только потом «уже в целевом статусе».
//
// Порядок — часть предиката: «приехала та же оплата» — это утверждение о ТЕХ ЖЕ
// деньгах, и событие с чужой суммой на оплаченном намерении обязано получить
// amount_mismatch, а не метку запоздалой доставки.
func checkApplyPredicate(in payment.Intent, req payment.ApplyEventRequest,
) (payment.ApplyOutcome, bool) {
	if in.Status != req.To && !slices.Contains(req.ExpectFrom, in.Status) {
		return payment.OutcomeStatusConflict, true
	}
	if req.Ledger != nil && !amountsAgree(in, req) {
		return payment.OutcomeAmountMismatch, true
	}
	if in.Status == req.To {
		return payment.OutcomeAlreadyInTarget, true
	}
	return "", false
}

// amountsAgree — сумма и валюта СТРОКИ намерения и СОБЫТИЯ равны ожидаемым.
// Сравнение через payment.Money: валюта — часть сравнения, и 79900 RUB не
// должны совпасть с 79900 KZT от провайдера, настроенного не на тот магазин.
func amountsAgree(in payment.Intent, req payment.ApplyEventRequest) bool {
	expected, err := payment.NewMoney(req.ExpectAmountMinor, req.ExpectCurrency)
	if err != nil {
		return false
	}
	stored, err := in.Money()
	if err != nil {
		return false
	}
	got, err := req.Event.Money()
	if err != nil {
		return false
	}
	return expected.Equal(stored) && expected.Equal(got)
}
