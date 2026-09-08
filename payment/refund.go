package payment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// RefundRequest — возврат оплаченной покупки, целиком или частью.
//
// СУММУ НАЗЫВАЕТ ВЫЗЫВАЮЩИЙ. Какие позиции возвращать, возвращать ли доставку и
// удерживать ли что-то — политика заказа, а не платежа: пакет не знает ни
// состава возврата, ни правил магазина. Он проверяет ровно одно, зато под
// блокировкой и триггером: 0 < AmountMinor ≤ нетто книги (Σcapture − Σrefund).
//
// Частичных возвратов на одно зачисление бывает несколько, и складываются они
// до потолка зачисления, а не до одной записи.
type RefundRequest struct {
	IntentID    uuid.UUID
	AmountMinor int64
	// IdempotencyKey — ключ ЭТОГО возврата. Тот же ключ с той же суммой —
	// повтор и успех; тот же ключ с другой суммой — 409. Разные ключи — разные
	// частичные возвраты, и суммируются они до нетто.
	IdempotencyKey string
	// ActorID — КТО возвращает. Обязателен: возврат это ручное движение денег,
	// и запись о нём без автора не отвечает на единственный вопрос, который
	// зададут при разборе, — «кто вернул эти деньги».
	ActorID uuid.UUID
	// Receipt — чек «возврат прихода» на сумму ЭТОГО возврата. СВОЙ, а не тот,
	// что уехал с покупкой: возврат денег — такой же расчёт, и без встречного
	// чека приход остаётся пробитым, а возврат нет.
	Receipt *Receipt
}

// RefundResult — компенсирующая запись и нетто по книге после неё.
type RefundResult struct {
	Entry LedgerEntry
	// Net — Σcapture − Σrefund после операции. Ноль означает, что вернули всё
	// зачисление; ненулевой остаток — что возврат был частичным.
	Net Money
}

// Refund возвращает деньги плательщику.
//
// Порядок намеренно такой: сначала провайдер, потом наша книга. При обратном
// порядке сбой у провайдера оставил бы книгу с возвратом, которого не было.
// Выбранный порядок в худшем случае оставляет деньги возвращёнными, а книгу
// отставшей — расхождение, которое чинит ЛЮБОЙ повтор с тем же клиентским
// ключом: ключ провайдера из него выведен (providerRefundKey), поэтому второй
// поход денег второй раз не двигает, а вставка записи защищена уникальным
// ключом строки книги.
func (s *Service) Refund(ctx context.Context, req RefundRequest) (RefundResult, Reason, error) {
	key, err := NormalizeKey(req.IdempotencyKey)
	if err != nil {
		return RefundResult{}, ReasonKeyInvalid, err
	}
	if req.ActorID == uuid.Nil {
		return RefundResult{}, ReasonNoActor,
			fmt.Errorf("%w: refund of intent %s", ErrNoActor, req.IntentID)
	}

	target, reason, err := s.refundable(ctx, req.IntentID)
	if err != nil {
		return RefundResult{}, reason, err
	}

	ledgerKey := refundLedgerKey(target.intent.ID, key)
	if prev, ok := findByLedgerKey(target.entries, ledgerKey); ok {
		return replayRefund(prev, req.AmountMinor, target.net)
	}
	if reason, err = target.affordable(req.AmountMinor); err != nil {
		return RefundResult{}, reason, err
	}
	if receiptErr := CheckReceipt(req.Receipt, s.cfg.RequireReceipt, req.AmountMinor); receiptErr != nil {
		return RefundResult{}, ReasonReceiptInvalid, receiptErr
	}

	ev, err := s.provider.Refund(ctx, RefundProviderRequest{
		ProviderPaymentID: target.intent.ProviderPaymentID,
		AmountMinor:       req.AmountMinor,
		Currency:          target.capture.Currency,
		IdempotencyKey:    providerRefundKey(s.cfg.ProviderKeyPrefix, target.intent.ID, key),
		Receipt:           req.Receipt,
	})
	if err != nil {
		return RefundResult{}, providerReason(err), fmt.Errorf("%w: provider refund: %w", ErrUnavailable, err)
	}
	if mismatch := checkRefundEcho(ev, req.AmountMinor, target.capture.Currency); mismatch != nil {
		return RefundResult{}, ReasonAmountMismatch, mismatch
	}
	return s.recordRefund(ctx, target, ev, req, ledgerKey)
}

// checkRefundEcho — провайдер вернул РОВНО ту сумму, которую мы просили.
//
// ОТКАЗ, который это ловит, единственный, но неисправимый. Ключ идемпотентности
// возврата выведен из намерения и клиентского ключа, а не из суммы, — так и
// надо, иначе повтор двигает деньги дважды. Но если первый поход к провайдеру
// УДАЛСЯ, а запись в книгу нет, и повтор пришёл с другой суммой, провайдер по
// тому же ключу вернёт ПЕРВЫЙ возврат. Запиши мы тогда в книгу свою новую
// цифру — книга разошлась бы с выпиской провайдера молча и навсегда: нетто
// выглядит правдоподобно, Drift ничего не видит, а обнаруживается это при
// сверке с банком.
//
// Поэтому расхождение — громкий отказ, а не выбор одной из двух цифр. Ни одна
// из них не годится: наша не соответствует деньгам, провайдерская не
// соответствует чеку, который уже пробит на нашу.
//
// Событие без суммы (провайдер её не называет) проверку проходит: сравнивать
// нечего, а требовать от адаптера поле, которого нет в ответе провайдера,
// значило бы заклинить возврат совсем.
func checkRefundEcho(ev Event, expectedMinor int64, currency string) error {
	if ev.AmountMinor == 0 && ev.Currency == "" {
		return nil
	}
	expected, err := NewMoney(expectedMinor, currency)
	if err != nil {
		return fmt.Errorf("%w: refund amount %d %s: %w", ErrAmountMismatch, expectedMinor, currency, err)
	}
	got, err := ev.Money()
	if err != nil {
		return fmt.Errorf("%w: provider refund event %s: %w", ErrAmountMismatch, ev.ProviderEventID, err)
	}
	if !expected.Equal(got) {
		return fmt.Errorf("%w: asked to refund %s, provider refunded %s", ErrAmountMismatch, expected, got)
	}
	return nil
}

// refundTarget — всё, что прочитано под возврат за один проход: намерение,
// погашаемая запись, книга целиком и нетто по ней. Одной структурой, чтобы
// книга читалась РОВНО раз: второе чтение той же книги в том же вызове дало бы
// две версии правды о том, сколько уже вернули.
type refundTarget struct {
	intent  Intent
	capture LedgerEntry
	entries []LedgerEntry
	net     Money
}

// affordable — сумма влезает в остаток. Последнее слово всё равно за книгой под
// блокировкой (Store.ApplyRefund), но отказ здесь приходит ДО похода к
// провайдеру, то есть до того, как деньги двинутся.
func (t refundTarget) affordable(amountMinor int64) (Reason, error) {
	money, err := NewMoney(amountMinor, t.intent.Currency)
	if err != nil {
		return ReasonRefundTooLarge, err
	}
	if money.IsZero() {
		// Нулевая строка в книге запрещена, нулевого чека не бывает, а
		// провайдеру нулевой возврат отправить нечем. Молчаливый успех сказал
		// бы оператору «вернули», не вернув ничего.
		return ReasonRefundTooLarge, fmt.Errorf("%w: refund of 0 is not a refund", ErrInvalidMoney)
	}
	if money.Minor() > t.net.Minor() {
		return ReasonRefundTooLarge, fmt.Errorf("%w: asked %s, intent %s has %s left",
			ErrRefundTooLarge, money, t.intent.ID, t.net)
	}
	return "", nil
}

// refundable проверяет, что возвращать есть что: намерение оплачено и в книге
// есть строка зачисления.
func (s *Service) refundable(ctx context.Context, intentID uuid.UUID) (refundTarget, Reason, error) {
	intent, found, err := s.store.IntentByID(ctx, intentID)
	if err != nil {
		return refundTarget{}, ReasonStoreError, fmt.Errorf("%w: load intent: %w", ErrUnavailable, err)
	}
	if !found {
		return refundTarget{}, ReasonUnknownIntent, fmt.Errorf("%w: %s", ErrUnknownIntent, intentID)
	}
	if intent.Status != StatusSucceeded {
		return refundTarget{}, ReasonNotSettled,
			fmt.Errorf("%w: intent %s is %s", ErrNotSettled, intentID, intent.Status)
	}

	entries, err := s.store.Ledger(ctx, intentID)
	if err != nil {
		return refundTarget{}, ReasonStoreError, fmt.Errorf("%w: load ledger: %w", ErrUnavailable, err)
	}
	capture, ok := findCapture(entries)
	if !ok {
		// Намерение оплачено, а денежной записи нет: это расхождение книг
		// (DriftSucceededNoCapture), а не «возврат нуля». Придумывать сумму
		// возврата из статуса — способ вернуть деньги, которых не получали.
		return refundTarget{}, ReasonNotSettled,
			fmt.Errorf("%w: intent %s has no capture entry", ErrNotSettled, intentID)
	}
	net, err := Net(entries, intent.Currency)
	if err != nil {
		return refundTarget{}, ReasonStoreError, err
	}
	return refundTarget{intent: intent, capture: capture, entries: entries, net: net}, "", nil
}

// replayRefund разбирает повтор по клиентскому ключу: та же сумма — успех,
// другая — попытка провести под одним ключом два разных возврата.
func replayRefund(prev LedgerEntry, amountMinor int64, net Money) (RefundResult, Reason, error) {
	if prev.AmountMinor != amountMinor {
		return RefundResult{Entry: prev}, ReasonKeyReused,
			fmt.Errorf("%w: key already refunded %d by entry %s",
				ErrIdempotencyKeyReused, prev.AmountMinor, prev.ID)
	}
	// Нетто уже включает эту запись: она в книге, по которой оно посчитано.
	return RefundResult{Entry: prev, Net: net}, ReasonReplay, nil
}

func (s *Service) recordRefund(ctx context.Context, target refundTarget, ev Event,
	req RefundRequest, ledgerKey string,
) (RefundResult, Reason, error) {
	now := s.now().UTC()
	captureID := target.capture.ID
	actor := req.ActorID
	entry := LedgerEntry{
		ID:       s.newID(),
		IntentID: target.intent.ID,
		Kind:     LedgerRefund,
		// Сумма положительная, а род несёт Kind: отрицательные суммы в книге
		// сделали бы «сколько получено» вопросом с двумя ответами.
		AmountMinor:     req.AmountMinor,
		Currency:        target.capture.Currency,
		ProviderEventID: ev.ProviderEventID,
		ReversesEntryID: &captureID,
		IdempotencyKey:  ledgerKey,
		ActorID:         &actor,
		CreatedAt:       now,
	}

	res, err := s.store.ApplyRefund(ctx, ApplyRefundRequest{
		IntentID:       target.intent.ID,
		CaptureEntryID: captureID,
		Refund:         entry,
		Now:            now,
	})
	if err != nil {
		return RefundResult{}, ReasonStoreError, fmt.Errorf("%w: apply refund: %w", ErrUnavailable, err)
	}
	switch res.Outcome {
	case OutcomeApplied:
		money, moneyErr := entry.Money()
		if moneyErr != nil {
			return RefundResult{Entry: res.Entry}, ReasonStoreError, moneyErr
		}
		return RefundResult{Entry: res.Entry, Net: target.net.Sub(money)}, ReasonRefunded, nil
	case OutcomeDuplicateEvent:
		// Гонка двух возвратов с одним клиентским ключом: победитель уже в
		// книге. Разбираем его тем же правилом «тот же ключ — повтор», что и до
		// похода к провайдеру.
		return replayRefund(res.Entry, req.AmountMinor, target.net)
	case OutcomeRefundTooLarge:
		// Деньги у провайдера уже ушли, а книга их не принимает: между нашей
		// проверкой и записью прошёл чужой возврат. Тихо этого оставлять
		// нельзя — 503 с алертом, дальше разбор человеком и сверка.
		return RefundResult{}, ReasonRefundTooLarge,
			fmt.Errorf("%w: provider refunded %d, ledger of intent %s refuses it",
				ErrUnavailable, req.AmountMinor, target.intent.ID)
	default:
		// Любой ДРУГОЙ неприменённый исход разбирать как повтор нельзя: у него
		// пустая res.Entry, и «уже возвращено ... entry 00000000-…» успокоило бы
		// оператора, хотя деньги ушли, а строки в книге нет. Наружу 503 с
		// именем исхода — это ретрай и алерт, а не тишина.
		return RefundResult{}, ReasonStoreError,
			fmt.Errorf("%w: apply refund returned %q", ErrUnavailable, res.Outcome)
	}
}

func findCapture(entries []LedgerEntry) (LedgerEntry, bool) {
	for _, e := range entries {
		if e.Kind == LedgerCapture {
			return e, true
		}
	}
	return LedgerEntry{}, false
}

func findByLedgerKey(entries []LedgerEntry, key string) (LedgerEntry, bool) {
	for _, e := range entries {
		if e.IdempotencyKey == key {
			return e, true
		}
	}
	return LedgerEntry{}, false
}
