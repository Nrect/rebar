package payment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Capture списывает холд: деньги, замороженные у плательщика, становятся нашими.
//
// ЧАСТИЧНОГО СПИСАНИЯ НЕТ. amountMinor обязан быть равен сумме намерения, и
// параметр существует ровно затем, чтобы вызывающий назвал сумму, которую он
// думает списать: сумма и состав заморожены в намерении (инвариант 7), а
// списание другой цифры разошлось бы и с составом, и с уже собранным чеком.
// Списать часть — это другое намерение на другую сумму.
//
// КЛИЕНТСКИЙ КЛЮЧ ЗДЕСЬ НЕ МЕХАНИЗМ ИДЕМПОТЕНТНОСТИ, и это стоит сказать прямо.
// Её дают две вещи, обе без участия вызывающего: производный ключ провайдера
// (одно списание на намерение) и дедуп события по (provider, event_id) — тот же
// id придёт и вебхуком, поэтому вторая строка в книгу не ляжет. Ключ
// проверяется как требование дисциплины: у того, кто двигает деньги, обязан
// быть собственный идентификатор попытки, иначе повтор после сбоя связи он
// отличить не сможет.
//
// Чек уезжает ИМЕННО СЮДА: у двухстадийной оплаты расчёт происходит в момент
// списания, а не постановки холда.
func (s *Service) Capture(ctx context.Context, intentID uuid.UUID, amountMinor int64,
	receipt *Receipt, key string,
) (Intent, Reason, error) {
	if _, err := NormalizeKey(key); err != nil {
		return Intent{}, ReasonKeyInvalid, err
	}
	intent, found, err := s.store.IntentByID(ctx, intentID)
	if err != nil {
		return Intent{}, ReasonStoreError, fmt.Errorf("%w: load intent: %w", ErrUnavailable, err)
	}
	if !found {
		return Intent{}, ReasonUnknownIntent, fmt.Errorf("%w: %s", ErrUnknownIntent, intentID)
	}
	// Холд уже списан — это УСПЕХ повтора, а не отказ: вызывающий, потерявший
	// наш ответ, обязан получить то же, что и в первый раз.
	if intent.Status == StatusSucceeded {
		return intent, ReasonReplay, nil
	}
	if reason, capErr := capturable(intent, amountMinor); capErr != nil {
		return intent, reason, capErr
	}
	if receiptErr := CheckReceipt(receipt, s.cfg.RequireReceipt, intent.AmountMinor); receiptErr != nil {
		return intent, ReasonReceiptInvalid, receiptErr
	}

	ev, err := s.provider.Capture(ctx, CaptureRequest{
		ProviderPaymentID: intent.ProviderPaymentID,
		AmountMinor:       intent.AmountMinor,
		Currency:          intent.Currency,
		IdempotencyKey:    providerCaptureKey(s.cfg.ProviderKeyPrefix, intent.ID),
		Receipt:           receipt,
	})
	if err != nil {
		return intent, providerReason(err), fmt.Errorf("%w: capture: %w", ErrUnavailable, err)
	}
	return s.applyAnswer(ctx, ev, intent)
}

// capturable — холд ещё можно списать (уже списанный разобран у вызывающего).
func capturable(in Intent, amountMinor int64) (Reason, error) {
	if in.Status != StatusAuthorized {
		return ReasonStatusConflict, fmt.Errorf("%w: intent %s is %s, not %s",
			ErrStatusConflict, in.ID, in.Status, StatusAuthorized)
	}
	if amountMinor != in.AmountMinor {
		// Расхождение не зачисляется ни в какую сторону (инвариант 8): меньше —
		// это частичное списание, которого в модели нет, больше — списание
		// сверх замороженного.
		return ReasonAmountMismatch, fmt.Errorf("%w: asked to capture %d, intent holds %d",
			ErrAmountMismatch, amountMinor, in.AmountMinor)
	}
	return "", nil
}

// Cancel снимает холд либо отменяет неоплаченный платёж: деньги плательщику не
// вернутся, потому что они и не списывались, — размораживается холд.
//
// Отмена намерения в created отвергается: платежа у провайдера может ещё не
// быть, а может и быть — ответ потерялся. Закрыть такую строку значило бы
// открыть окно, в котором на отменённое намерение приходят деньги. Ждать
// нужно pending (тогда есть что отменять) или TTL.
func (s *Service) Cancel(ctx context.Context, intentID uuid.UUID, key string) (Intent, Reason, error) {
	if _, err := NormalizeKey(key); err != nil {
		return Intent{}, ReasonKeyInvalid, err
	}
	intent, found, err := s.store.IntentByID(ctx, intentID)
	if err != nil {
		return Intent{}, ReasonStoreError, fmt.Errorf("%w: load intent: %w", ErrUnavailable, err)
	}
	if !found {
		return Intent{}, ReasonUnknownIntent, fmt.Errorf("%w: %s", ErrUnknownIntent, intentID)
	}
	// Уже отменённое намерение — повтор и успех, по той же причине, что и у
	// списанного холда.
	if intent.Status == StatusCanceled {
		return intent, ReasonReplay, nil
	}
	if reason, cancelErr := cancelable(intent); cancelErr != nil {
		return intent, reason, cancelErr
	}

	ev, err := s.provider.Cancel(ctx, intent.ProviderPaymentID,
		providerCancelKey(s.cfg.ProviderKeyPrefix, intent.ID))
	if err != nil {
		return intent, providerReason(err), fmt.Errorf("%w: cancel: %w", ErrUnavailable, err)
	}
	return s.applyAnswer(ctx, ev, intent)
}

// cancelable — есть ли у провайдера что отменять (уже отменённое разобрано у
// вызывающего).
func cancelable(in Intent) (Reason, error) {
	if in.Status.IsTerminal() {
		return ReasonIntentClosed, fmt.Errorf("%w: intent %s is %s", ErrIntentClosed, in.ID, in.Status)
	}
	if in.ProviderPaymentID == "" {
		return ReasonInvalidRequest, fmt.Errorf(
			"%w: intent %s has no payment at the provider yet; retry once it is %s",
			ErrInvalidRequest, in.ID, StatusPending)
	}
	return "", nil
}

// applyAnswer применяет ответ провайдера на НАШ вызов тем же путём, что и
// вебхук: тот же дедуп, тот же предикат, та же книга.
//
// Привязка события к намерению здесь безопасна — мы спрашивали про КОНКРЕТНЫЙ
// платёж, id которого сами и сохранили, — в отличие от вебхука, где intent_id
// приходит из metadata и доверять ему нельзя. Но ответ ПРО ДРУГОЙ платёж не
// применяется: это либо перепутанные магазины, либо баг адаптера, и в обоих
// случаях зачисление ушло бы не тому.
func (s *Service) applyAnswer(ctx context.Context, ev Event, intent Intent) (Intent, Reason, error) {
	if ev.ProviderPaymentID != "" && ev.ProviderPaymentID != intent.ProviderPaymentID {
		return intent, ReasonMalformedEvent, fmt.Errorf("%w: asked about %s, got %s",
			ErrMalformedEvent, intent.ProviderPaymentID, ev.ProviderPaymentID)
	}
	ev.IntentID = intent.ID

	res, reason, err := s.apply(ctx, ev)
	if err != nil {
		return intent, reason, err
	}
	return res.Intent, reason, nil
}
