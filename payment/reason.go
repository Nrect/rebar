package payment

// Reason — почему исход такой. Закрытый набор значений: годится меткой метрики
// payments_total{op,reason}, и кардинальность не растёт ни от каталога
// потребителя, ни от текстов провайдера.
//
// Значения snake_case: метка метрики не должна разъезжаться в регистре, иначе
// один и тот же исход станет двумя рядами на дашборде.
//
// Значения «неизвестно» нет намеренно: неразобранный исход — это не особый
// случай, а потерянная метрика.
type Reason string

const (
	// Покупка.
	ReasonCreated          Reason = "created"           // намерение создано, ссылка выдана
	ReasonReplay           Reason = "replay"            // тот же ключ, та же операция — прежний результат
	ReasonKeyReused        Reason = "key_reused"        // тот же ключ, ДРУГАЯ операция
	ReasonKeyInvalid       Reason = "key_invalid"       // ключ пуст/длинный/непечатный
	ReasonInvalidRequest   Reason = "invalid_request"   // состав, сумма, способ оплаты или ссылка негодны
	ReasonReferenceBusy    Reason = "reference_busy"    // у ссылки потребителя уже есть живое намерение
	ReasonProviderRejected Reason = "provider_rejected" // провайдер отказал детерминированно
	ReasonProviderError    Reason = "provider_error"    // провайдер не ответил
	ReasonUnsupported      Reason = "unsupported"       // адаптер провайдера операции не умеет
	ReasonIntentClosed     Reason = "intent_closed"     // попытка закрыта, нужен новый ключ
	// ReasonReceiptInvalid — чека нет либо он не сходится с суммой расчёта.
	// Отдельная причина: всплеск по ней означает не «товар не продаётся», а
	// «продажи встали из-за чека», и разбирают их разные люди.
	ReasonReceiptInvalid Reason = "receipt_invalid"

	// Вебхук, холд и сверка.
	ReasonSettled          Reason = "settled"           // деньги учтены
	ReasonAuthorized       Reason = "authorized"        // холд поставлен, деньги ещё не списаны
	ReasonCanceled         Reason = "canceled"          // холд снят либо платёж отменён
	ReasonDuplicateEvent   Reason = "duplicate_event"   // событие уже применяли
	ReasonIgnoredEvent     Reason = "ignored_event"     // событие записано, но домен его не применяет
	ReasonUnknownIntent    Reason = "unknown_intent"    // событие на намерение, которого нет
	ReasonAmountMismatch   Reason = "amount_mismatch"   // провайдер назвал не ту сумму
	ReasonLateEvent        Reason = "late_event"        // порядок доставки нарушен, даунгрейда нет
	ReasonStatusConflict   Reason = "status_conflict"   // деньги на закрытом намерении — разбор руками
	ReasonSignatureInvalid Reason = "signature_invalid" // вебхук не подтверждён
	ReasonMalformedEvent   Reason = "malformed_event"   // тело не разбирается
	ReasonExpired          Reason = "expired"           // TTL истёк, платежа у провайдера нет
	ReasonStillPending     Reason = "still_pending"     // провайдер считает платёж живым

	// Возврат.
	ReasonRefunded   Reason = "refunded"
	ReasonNotSettled Reason = "not_settled"
	// ReasonRefundTooLarge — просят вернуть больше, чем осталось зачислено.
	// Своя метка, а не amount_mismatch: там провайдер назвал чужую сумму, здесь
	// оператор просит больше нетто, и разбирают их разные люди.
	ReasonRefundTooLarge Reason = "refund_too_large"
	// ReasonNoActor — ручной возврат пришёл без автора. Ошибка транспорта, а не
	// пользователя.
	ReasonNoActor Reason = "no_actor"

	// Инфраструктура.
	ReasonStoreError Reason = "store_error" // решение не принято
)

// AllReasons — полный список. Держит guard-тест: значение, добавленное мимо
// списка, не попадёт в документацию алертов, и всплеск по нему никто не увидит.
var AllReasons = []Reason{
	ReasonCreated,
	ReasonReplay,
	ReasonKeyReused,
	ReasonKeyInvalid,
	ReasonInvalidRequest,
	ReasonReferenceBusy,
	ReasonProviderRejected,
	ReasonProviderError,
	ReasonUnsupported,
	ReasonIntentClosed,
	ReasonReceiptInvalid,
	ReasonSettled,
	ReasonAuthorized,
	ReasonCanceled,
	ReasonDuplicateEvent,
	ReasonIgnoredEvent,
	ReasonUnknownIntent,
	ReasonAmountMismatch,
	ReasonLateEvent,
	ReasonStatusConflict,
	ReasonSignatureInvalid,
	ReasonMalformedEvent,
	ReasonExpired,
	ReasonStillPending,
	ReasonRefunded,
	ReasonNotSettled,
	ReasonRefundTooLarge,
	ReasonNoActor,
	ReasonStoreError,
}
