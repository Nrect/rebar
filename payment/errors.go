package payment

import "errors"

// Sentinel-ошибки пакета. Вызывающий ветвится по ним через errors.Is; текст
// наружу не уходит — HTTP-код и slug выбирает транспорт по паре (ошибка, Reason).
var (
	// ErrIdempotencyKeyInvalid — ключ пуст после нормализации, длиннее
	// MaxIdempotencyKeyLen, не UTF-8 или содержит непечатное. Наружу 400: это
	// ошибка вызывающего, и молчать о ней нельзя — операция не выполнена.
	ErrIdempotencyKeyInvalid = errors.New("idempotency key is empty, too long or not printable")
	// ErrIdempotencyKeyReused — тот же ключ на ДРУГУЮ операцию. Наружу 409.
	// Тихий no-op здесь скрыл бы, что вторая (реально другая) покупка не
	// состоялась, и отдал бы клиенту чужую ссылку на оплату вместо своей.
	ErrIdempotencyKeyReused = errors.New("idempotency key was used for a different operation")
	// ErrIdempotencyRace — гонку за ключ проиграли (нарушен уникальный индекс
	// (payer_id, idempotency_key)). Наружу НЕ уходит: сервис перечитывает
	// победителя и отдаёт его результат. Отдельный тип нужен, чтобы адаптер не
	// сворачивал ЛЮБОЕ нарушение уникальности в «повтор»: нарушение другого
	// индекса означает совсем другое.
	ErrIdempotencyRace = errors.New("lost the race for this idempotency key")
	// ErrReferenceBusy — у этой ссылки потребителя уже есть живое намерение.
	// Наружу 409: у заказа одна открытая попытка оплаты, и вторая означала бы
	// два платежа за один заказ. Новую заводят после того, как первая стала
	// терминальной.
	ErrReferenceBusy = errors.New("reference already has a live payment intent")

	// ErrInvalidRequest — покупка не собирается: нет плательщика, негодная
	// ссылка потребителя, состав не сходится с итогом или способ оплаты не из
	// Config.Methods.
	ErrInvalidRequest = errors.New("purchase request is not sellable as given")
	// ErrInvalidMoney — сумма отрицательна, превышает потолок или пришла в чужой
	// валюте.
	ErrInvalidMoney = errors.New("amount must be positive, within the cap, and in the configured currency")
	// ErrBadStatus — незнакомый статус: строка из БД, которую пакет не знает.
	ErrBadStatus = errors.New("unknown payment status")
	// ErrBadTransition — переход запрещён таблицей либо предикат собран пустым.
	// Это ошибка программиста, а не пользователя: наружу 500.
	ErrBadTransition = errors.New("status transition is not allowed")
	// ErrStatusConflict — событие пришло на статус, к которому оно неприменимо.
	// Опасные клетки (деньги на отменённом/протухшем намерении) разбирает
	// человек по алерту, поэтому провайдеру всё равно отвечаем 200.
	ErrStatusConflict = errors.New("intent is in a status this event cannot be applied to")
	// ErrIntentClosed — попытка закрыта (canceled/expired/failed), продолжать её
	// нечем: нужен НОВЫЙ ключ идемпотентности.
	//
	// Отдельно от ErrStatusConflict намеренно: у status_conflict алерт с порогом
	// 1 («деньги пришли на списанное намерение»), и подмешивать туда штатный
	// ретрай ключа с отменённой вкладки — это тренировать дежурного не смотреть
	// на алерт.
	ErrIntentClosed = errors.New("payment attempt is closed; a new idempotency key is required")
	// ErrUnknownIntent — событие или запрос ссылается на намерение, которого у
	// нас нет.
	ErrUnknownIntent = errors.New("operation references an intent we do not have")
	// ErrAmountMismatch — провайдер назвал не ту сумму или не ту валюту.
	ErrAmountMismatch = errors.New("provider amount disagrees with the intent")

	// ErrReceiptRequired — расчёт без чека при включённом требовании чека.
	// Наружу 500: чек собирает потребитель из каталога и профиля, и его
	// отсутствие — ошибка сборки, а не пользователя.
	//
	// Отказ ДО похода к провайдеру и до вставки намерения: платёж, созданный
	// без чека, уже не исправить — расчёт состоялся, фискального документа на
	// него нет, и дописать его нечем.
	ErrReceiptRequired = errors.New("a fiscal receipt is required for this sale")
	// ErrReceiptInvalid — чек есть, но негоден: сумма строк не равна сумме
	// расчёта, нет ни почты, ни телефона, строка без наименования, количества
	// или ставки НДС.
	ErrReceiptInvalid = errors.New("fiscal receipt does not match the payment")

	// ErrInvalidSignature — вебхук не подтверждён: подпись не сошлась либо
	// запрос пришёл не с адреса провайдера. Наружу 400: это не наш провайдер,
	// и ретрай ему не поможет.
	ErrInvalidSignature = errors.New("webhook is not authentic")
	// ErrMalformedEvent — тело не разбирается либо разобралось в непригодное
	// событие (пустой id события, чужой провайдер).
	ErrMalformedEvent = errors.New("webhook payload is not a usable event")

	// ErrProviderRejected — провайдер отказался создавать платёж.
	// Детерминированный отказ, а не сбой связи: намерение переводится в failed,
	// и повтор того же ключа возвращает тот же отказ.
	ErrProviderRejected = errors.New("provider refused to create the payment")
	// ErrUnsupported — адаптер провайдера этой операции не умеет (холдов нет,
	// возвратов нет). Отдельно от ErrProviderRejected: отказ навсегда и по
	// конструкции, ретрай и разбор с провайдером бессмысленны.
	ErrUnsupported = errors.New("provider does not support this operation")

	// ErrNoActor — ручное движение денег пришло без автора. Наружу 500: это не
	// ошибка пользователя, а транспорт, не проставивший, КТО нажал кнопку.
	//
	// Отказ громкий и до похода к провайдеру: денежная строка без автора
	// оставляет вопрос «кто вернул эти деньги» без ответа навсегда.
	ErrNoActor = errors.New("a manual money movement requires an actor")

	// ErrNotSettled — возврат просят по неоплаченному намерению.
	ErrNotSettled = errors.New("only a settled payment can be refunded")
	// ErrRefundTooLarge — сумма возврата больше нетто по книге
	// (Σcapture − Σrefund). Наружу 409: отказ по существу, а не сбой.
	//
	// Частичные возвраты законны и складываются, поэтому «уже возвращено»
	// перестало быть да/нет: единственный потолок — нетто, и он же стоит
	// триггером в книге.
	ErrRefundTooLarge = errors.New("refund exceeds the amount still captured")

	// ErrUnavailable — решение не принято (сбой стора или провайдера). Наружу
	// 503, а не 500 и не 200: провайдеру нужен ретрай, клиенту — «попробуйте
	// позже», а инциденту — не утонуть среди штатных отказов.
	//
	// 200 на сбой БД теряет оплату НАВСЕГДА: провайдер считает вебхук
	// доставленным и больше не придёт.
	ErrUnavailable = errors.New("payment operation could not be completed")
)
