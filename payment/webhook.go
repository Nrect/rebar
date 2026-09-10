package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// WebhookResult — что стало с событием.
type WebhookResult struct {
	Event   Event
	Outcome ApplyOutcome
	Intent  Intent
}

// HandleWebhook подтверждает уведомление провайдера и применяет событие.
//
// КОНТРАКТ ОШИБОК. Ошибка возвращается ТОЛЬКО тогда, когда провайдеру нельзя
// отвечать 200:
//   - ErrInvalidSignature и ErrMalformedEvent — 400: это не наш провайдер либо
//     тело нечитаемо, ретрай не поможет. ErrMalformedEvent — только ЯВНЫЙ:
//     названный адаптером либо найденный проверкой разобранного события;
//   - ErrUnavailable — 503: ретрай осмыслен. Сюда же уходит любая
//     неклассифицированная ошибка адаптера (см. parseError).
//
// Всё остальное (дубль, запоздалое событие, орфан, расхождение сумм,
// конфликт статусов) возвращается с nil-ошибкой и говорящим Reason: провайдеру
// нужен 200, а дежурному — алерт по метке.
//
// Асимметрия сознательная и стоит отдельного абзаца, потому что обе ошибки
// одинаково легко сделать и обе дорогие: 200 на сбой БД теряет оплату НАВСЕГДА
// (провайдер посчитает вебхук доставленным), а 4xx на дубликат заставляет
// провайдера ретраить вечно то, что уже учтено, — и после серии неуспехов
// провайдеры отключают приёмник, то есть перестают доходить и нужные события.
func (s *Service) HandleWebhook(ctx context.Context, req WebhookRequest) (WebhookResult, Reason, error) {
	// Подлинность проверяется первой и БЕЗ похода в БД: неподтверждённый запрос
	// не должен стоить нам ни одного соединения из пула.
	ev, err := s.provider.ParseWebhook(ctx, req)
	if err != nil {
		reason, parseErr := parseError(err)
		return WebhookResult{}, reason, parseErr
	}
	return s.apply(ctx, ev)
}

// parseError — причина и ошибка для отказа ParseWebhook.
//
// НЕУВЕРЕННОСТЬ ПАДАЕТ В СТОРОНУ ПОВТОРА. 400 провайдер читает как «доставлено,
// больше не присылай», 503 — как «повтори позже». Ошибочный 503 стоит
// ограниченного повтора заведомо плохого тела; ошибочный 400 стоит оплаты:
// провайдер больше не придёт, и деньги останутся не зачисленными. Поэтому 400 —
// только по явному классу, а неклассифицированная ошибка — например, сырой
// таймаут проверочного чтения у адаптера, забывшего его обернуть, — это
// недоступность. По той же причине ErrUnavailable проверяется раньше
// ErrMalformedEvent.
func parseError(err error) (Reason, error) {
	if errors.Is(err, ErrInvalidSignature) {
		return ReasonSignatureInvalid, err
	}
	if errors.Is(err, ErrUnavailable) {
		return ReasonProviderError, err
	}
	if errors.Is(err, ErrMalformedEvent) {
		return ReasonMalformedEvent, err
	}
	return ReasonProviderError, fmt.Errorf("%w: parse webhook: %w", ErrUnavailable, err)
}

// apply — ЕДИНСТВЕННЫЙ путь применения события: и для вебхука, и для сверки, и
// для ответа на списание холда.
//
// Отдельной ветки «применить из сверки» нет намеренно: вторая реализация
// зачисления разъехалась бы с первой на первой же правке, и разъехалась бы
// молча.
func (s *Service) apply(ctx context.Context, ev Event) (WebhookResult, Reason, error) {
	if err := s.validateEvent(ev); err != nil {
		return WebhookResult{Event: ev}, ReasonMalformedEvent, err
	}
	// Орфан: событие ссылается на намерение, которого у нас нет. Строку события
	// всё равно пишем — орфан обязан быть видимым, а не потерянным, — но
	// намерение по событию НЕ создаём: «зачислим по данным, которых у нас нет»
	// это отдать товар всякому, кто умеет прислать событие.
	if ev.IntentID == uuid.Nil {
		return s.record(ctx, ev, uuid.Nil, ReasonUnknownIntent)
	}

	intent, found, err := s.store.IntentByID(ctx, ev.IntentID)
	if err != nil {
		return WebhookResult{Event: ev}, ReasonStoreError,
			fmt.Errorf("%w: load intent: %w", ErrUnavailable, err)
	}
	if !found {
		return s.record(ctx, ev, ev.IntentID, ReasonUnknownIntent)
	}

	target, applicable := targetStatus(ev.Type)
	if !applicable {
		return s.record(ctx, ev, intent.ID, ReasonIgnoredEvent)
	}
	return s.applyTo(ctx, ev, intent, target)
}

func (s *Service) applyTo(ctx context.Context, ev Event, intent Intent, target Status,
) (WebhookResult, Reason, error) {
	req := ApplyEventRequest{
		IntentID: intent.ID,
		Event:    ev,
		// Предикат считает домен по таблице переходов, а не адаптер: ExpectFrom
		// это ВСЕ законные источники целевого статуса, а не «тот статус, что мы
		// только что прочитали». Прочитанный статус устарел бы к моменту
		// блокировки, и check-then-act вернулся бы через заднюю дверь.
		ExpectFrom: statusesInto(target),
		To:         target,
		Now:        s.now().UTC(),
	}
	if target == StatusSucceeded {
		req.ExpectAmountMinor = intent.AmountMinor
		req.ExpectCurrency = intent.Currency
		req.Ledger = s.captureEntry(ev, intent, req.Now)
	}

	res, err := s.store.ApplyEvent(ctx, req)
	if err != nil {
		return WebhookResult{Event: ev, Intent: intent}, ReasonStoreError,
			fmt.Errorf("%w: apply event: %w", ErrUnavailable, err)
	}
	return WebhookResult{Event: ev, Outcome: res.Outcome, Intent: res.Intent},
		classifyOutcome(res.Outcome, res.Intent.Status, target), nil
}

// captureEntry — строка зачисления.
//
// Сумма берётся из СНАПШОТА намерения, а не из события: то, что они равны,
// доказывает предикат стора под блокировкой, и при расхождении записи не будет
// вовсе. Взять сумму из события значило бы зачислить то, что прислали.
func (s *Service) captureEntry(ev Event, intent Intent, now time.Time) *LedgerEntry {
	return &LedgerEntry{
		ID:              s.newID(),
		IntentID:        intent.ID,
		Kind:            LedgerCapture,
		AmountMinor:     intent.AmountMinor,
		Currency:        intent.Currency,
		ProviderEventID: ev.ProviderEventID,
		IdempotencyKey:  captureLedgerKey(intent.ID, ev.ProviderEventID),
		CreatedAt:       now,
	}
}

// record записывает событие, ничего не применяя, и возвращает fallback как
// причину — если только это не оказался дубль уже виденного события.
func (s *Service) record(ctx context.Context, ev Event, intentID uuid.UUID, fallback Reason,
) (WebhookResult, Reason, error) {
	res, err := s.store.ApplyEvent(ctx, ApplyEventRequest{
		IntentID: intentID,
		Event:    ev,
		Now:      s.now().UTC(),
	})
	if err != nil {
		return WebhookResult{Event: ev}, ReasonStoreError,
			fmt.Errorf("%w: record event: %w", ErrUnavailable, err)
	}
	reason := fallback
	if res.Outcome == OutcomeDuplicateEvent {
		reason = ReasonDuplicateEvent
	}
	return WebhookResult{Event: ev, Outcome: res.Outcome, Intent: res.Intent}, reason, nil
}

func (s *Service) validateEvent(ev Event) error {
	if ev.ProviderEventID == "" {
		// Событие без собственного id невозможно дедуплицировать, а значит его
		// повторная доставка зачислила бы деньги дважды.
		return fmt.Errorf("%w: event has no provider event id", ErrMalformedEvent)
	}
	if ev.Provider != s.provider.Name() {
		return fmt.Errorf("%w: event from provider %q, service serves %q",
			ErrMalformedEvent, ev.Provider, s.provider.Name())
	}
	if !ev.Type.valid() {
		return fmt.Errorf("%w: unknown event type %q", ErrMalformedEvent, ev.Type)
	}
	return nil
}

// targetStatus — в какой статус ведёт событие. Второй результат false означает
// «домен это событие не применяет».
//
// EventRefunded сюда не отображается намеренно: возврат, начатый в кабинете
// провайдера, — это движение денег мимо нашей книги, и автоматически списывать
// его пакет не станет. Событие записывается, вылезает расхождением в сверке и
// разбирается человеком; тихая правка книги по внешнему событию была бы
// решением, которого никто не принимал.
func targetStatus(t EventType) (Status, bool) {
	switch t {
	case EventSucceeded:
		return StatusSucceeded, true
	case EventAuthorized:
		return StatusAuthorized, true
	case EventCanceled:
		return StatusCanceled, true
	case EventFailed:
		return StatusFailed, true
	case EventPending:
		return StatusPending, true
	case EventRefunded, EventIgnored:
		return "", false
	default:
		return "", false
	}
}

// classifyOutcome превращает исход стора в метку метрики.
func classifyOutcome(out ApplyOutcome, current, target Status) Reason {
	switch out {
	case OutcomeApplied:
		return appliedReason(target)
	case OutcomeDuplicateEvent:
		return ReasonDuplicateEvent
	case OutcomeUnknownIntent:
		return ReasonUnknownIntent
	case OutcomeAmountMismatch:
		return ReasonAmountMismatch
	// Намерение уже в целевом статусе: та же оплата приехала вторым событием с
	// другим id. Это норма at-least-once доставки, а не сбой.
	case OutcomeAlreadyInTarget:
		return ReasonLateEvent
	case OutcomeStatusConflict:
		return classifyConflict(current, target)
	case OutcomeIgnored:
		return ReasonIgnoredEvent
	case OutcomeRefundTooLarge:
		return ReasonRefundTooLarge
	default:
		return ReasonStoreError
	}
}

func appliedReason(target Status) Reason {
	switch target {
	case StatusSucceeded:
		return ReasonSettled
	case StatusAuthorized:
		return ReasonAuthorized
	case StatusPending:
		return ReasonStillPending
	case StatusCanceled:
		return ReasonCanceled
	case StatusFailed, StatusExpired:
		return ReasonIntentClosed
	case StatusCreated:
		return ReasonStoreError
	default:
		return ReasonStoreError
	}
}

// classifyConflict отделяет безобидный беспорядок доставки от инцидента.
//
// Громкими считаются ровно две ситуации, и обе означают, что деньги и товар
// разошлись:
//   - событие об УСПЕХЕ пришло на статус, из которого зачисление незаконно
//     (created, canceled, expired, failed). Тихо зачислить нельзя — значит, TTL
//     и отмену можно обойти, придержав вебхук; тихо отказать нельзя — человек
//     заплатил. Разбирает человек по алерту;
//   - отмена/отказ/протухание пришли на УЖЕ ОПЛАЧЕННОЕ намерение.
//
// Всё остальное — запоздалый даунгрейд (pending после succeeded, отказ после
// отмены): статус не меняется, деньги не двигаются, 200 и никакого алерта.
func classifyConflict(current, target Status) Reason {
	if target == StatusSucceeded {
		return ReasonStatusConflict
	}
	if current == StatusSucceeded && target != StatusPending {
		return ReasonStatusConflict
	}
	return ReasonLateEvent
}
