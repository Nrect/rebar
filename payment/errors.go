package payment

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки пакета. Вызывающий ветвится по ним через errors.Is; класс для
// HTTP-статуса несёт сама sentinel (ADR-0007), слаг выбирает потребитель, текст
// наружу не уходит. Префикс «payment:» в тексте обязателен: KindError равны по
// классу и тексту, и sentinel другого модуля с тем же классом и текстом
// совпала бы с нашей через errors.Is.
var (
	// ErrIdempotencyKeyInvalid — ключ пуст после нормализации, длиннее
	// MaxIdempotencyKeyLen, не UTF-8 или содержит непечатное. Наружу 400: это
	// ошибка вызывающего, и молчать о ней нельзя — операция не выполнена.
	ErrIdempotencyKeyInvalid = errs.Kinded(errs.KindIncorrectInput,
		"payment: idempotency key is empty, too long or not printable")
	// ErrIdempotencyKeyReused — тот же ключ на ДРУГУЮ операцию. Наружу 409.
	// Тихий no-op здесь скрыл бы, что вторая (реально другая) покупка не
	// состоялась, и отдал бы клиенту чужую ссылку на оплату вместо своей.
	ErrIdempotencyKeyReused = errs.Kinded(errs.KindConflict,
		"payment: idempotency key was used for a different operation")
	// ErrIdempotencyRace — гонку за ключ проиграли (нарушен уникальный индекс
	// (payer_id, idempotency_key)). Наружу НЕ уходит: сервис перечитывает
	// победителя и отдаёт его результат. Отдельный тип нужен, чтобы адаптер не
	// сворачивал ЛЮБОЕ нарушение уникальности в «повтор»: нарушение другого
	// индекса означает совсем другое. Класс 409 (ключ занял параллельный
	// запрос) виден только тому, кто зовёт стор мимо сервиса.
	ErrIdempotencyRace = errs.Kinded(errs.KindConflict, "payment: lost the race for this idempotency key")
	// ErrReferenceBusy — у этой ссылки потребителя уже есть живое намерение.
	// Наружу 409: у заказа одна открытая попытка оплаты, и вторая означала бы
	// два платежа за один заказ. Новую заводят после того, как первая стала
	// терминальной.
	ErrReferenceBusy = errs.Kinded(errs.KindConflict, "payment: reference already has a live payment intent")

	// ErrInvalidRequest — покупка не собирается: нет плательщика, негодная
	// ссылка потребителя, состав не сходится с итогом или способ оплаты не из
	// Config.Methods; у Cancel — платежа у провайдера ещё нет. Наружу 400:
	// главный путь — запрос покупки.
	ErrInvalidRequest = errs.Kinded(errs.KindIncorrectInput, "payment: purchase request is not sellable as given")
	// ErrInvalidMoney — сумма отрицательна, превышает потолок или пришла в чужой
	// валюте. Наружу 400: главный путь — запрос покупки и возврата.
	ErrInvalidMoney = errs.Kinded(errs.KindIncorrectInput,
		"payment: amount must be positive, within the cap, and in the configured currency")
	// ErrBadStatus — незнакомый статус: строка из БД, которую пакет не знает.
	//errs:nokind статус приходит из базы, а не от клиента: незнакомый — схема и код разошлись при выкате, то есть 500
	ErrBadStatus = errors.New("payment: unknown payment status")
	// ErrBadTransition — переход запрещён таблицей либо предикат собран пустым.
	// Это ошибка программиста, а не пользователя: наружу 500.
	//errs:nokind переход и предикат собирает код, а не клиент: ошибка программиста, то есть 500
	ErrBadTransition = errors.New("payment: status transition is not allowed")
	// ErrStatusConflict — операция или событие пришли на статус, к которому они
	// неприменимы. Ошибкой её отдаёт Capture (холд не в том статусе): наружу
	// 409. Вебхук её не возвращает: опасные клетки (деньги на отменённом или
	// протухшем намерении) разбирает человек по алерту, и провайдеру всё равно
	// отвечаем 200.
	ErrStatusConflict = errs.Kinded(errs.KindConflict, "payment: intent is in a status this event cannot be applied to")
	// ErrIntentClosed — попытка закрыта (canceled/expired/failed), продолжать её
	// нечем: нужен НОВЫЙ ключ идемпотентности. Наружу 409.
	//
	// Отдельно от ErrStatusConflict намеренно: у status_conflict алерт с порогом
	// 1 («деньги пришли на списанное намерение»), и подмешивать туда штатный
	// ретрай ключа с отменённой вкладки — это тренировать дежурного не смотреть
	// на алерт.
	ErrIntentClosed = errs.Kinded(errs.KindConflict, "payment: attempt is closed; a new idempotency key is required")
	// ErrUnknownIntent — запрос ссылается на намерение, которого у нас нет.
	// Наружу 404; событие-орфан ошибки не получает — оно записывается с Reason.
	ErrUnknownIntent = errs.Kinded(errs.KindNotFound, "payment: operation references an intent we do not have")
	// ErrAmountMismatch — сумма или валюта разошлись с намерением: у Capture —
	// названная вызывающим, у Refund — эхо провайдера.
	//errs:nokind у Capture это ввод оператора, у Refund — аномалия провайдера, и 4xx оператору был бы ложью «вы ошиблись» (ADR-0007)
	ErrAmountMismatch = errors.New("payment: provider amount disagrees with the intent")

	// ErrReceiptRequired — расчёт без чека при включённом требовании чека.
	// Наружу 500: чек собирает потребитель из каталога и профиля, и его
	// отсутствие — ошибка сборки, а не пользователя.
	//
	// Отказ ДО похода к провайдеру и до вставки намерения: платёж, созданный
	// без чека, уже не исправить — расчёт состоялся, фискального документа на
	// него нет, и дописать его нечем.
	//errs:nokind чек собирает код потребителя из каталога и профиля: его отсутствие — ошибка сборки, то есть 500
	ErrReceiptRequired = errors.New("payment: a fiscal receipt is required for this sale")
	// ErrReceiptInvalid — чек есть, но негоден: сумма строк не равна сумме
	// расчёта, нет ни почты, ни телефона, строка без наименования, количества
	// или ставки НДС.
	//errs:nokind половина путей — данные покупателя, половина — сборка чека кодом потребителя: дефект сборки под 400 спрятался бы (ADR-0007)
	ErrReceiptInvalid = errors.New("payment: fiscal receipt does not match the payment")

	// ErrInvalidSignature — вебхук не подтверждён: подпись не сошлась либо
	// запрос пришёл не с адреса провайдера. Наружу 400: это не наш провайдер,
	// и ретрай ему не поможет.
	ErrInvalidSignature = errs.Kinded(errs.KindIncorrectInput, "payment: webhook is not authentic")
	// ErrMalformedEvent — тело не разбирается либо разобралось в непригодное
	// событие (пустой id события, чужой провайдер). Наружу 400: ретрай тела не
	// поможет (контракт HandleWebhook).
	ErrMalformedEvent = errs.Kinded(errs.KindIncorrectInput, "payment: webhook payload is not a usable event")

	// ErrProviderRejected — провайдер отказался создавать платёж.
	// Детерминированный отказ, а не сбой связи: намерение переводится в failed,
	// и повтор того же ключа возвращает тот же отказ. Наружу 409.
	ErrProviderRejected = errs.Kinded(errs.KindConflict, "payment: provider refused to create the payment")
	// ErrUnsupported — адаптер провайдера этой операции не умеет (холдов нет,
	// возвратов нет). Отдельно от ErrProviderRejected: отказ навсегда и по
	// конструкции, ретрай и разбор с провайдером бессмысленны. Наружу 501:
	// клиенты не повторяют его так, как 503.
	ErrUnsupported = errs.Kinded(errs.KindNotImplemented, "payment: provider does not support this operation")

	// ErrNoActor — ручное движение денег пришло без автора. Наружу 500: это не
	// ошибка пользователя, а транспорт, не проставивший, КТО нажал кнопку.
	//
	// Отказ громкий и до похода к провайдеру: денежная строка без автора
	// оставляет вопрос «кто вернул эти деньги» без ответа навсегда.
	//errs:nokind автора проставляет транспорт потребителя, а не пользователь: его отсутствие — дефект кода, то есть 500
	ErrNoActor = errors.New("payment: a manual money movement requires an actor")

	// ErrNotSettled — возврат просят по неоплаченному намерению. Наружу 409.
	ErrNotSettled = errs.Kinded(errs.KindConflict, "payment: only a settled payment can be refunded")
	// ErrRefundTooLarge — сумма возврата больше нетто по книге
	// (Σcapture − Σrefund). Наружу 409: отказ по существу, а не сбой.
	//
	// Частичные возвраты законны и складываются, поэтому «уже возвращено»
	// перестало быть да/нет: единственный потолок — нетто, и он же стоит
	// триггером в книге.
	ErrRefundTooLarge = errs.Kinded(errs.KindConflict, "payment: refund exceeds the amount still captured")

	// ErrUnavailable — решение не принято (сбой стора или провайдера). Наружу
	// 503, а не 500 и не 200: провайдеру нужен ретрай, клиенту — «попробуйте
	// позже», а инциденту — не утонуть среди штатных отказов.
	//
	// 200 на сбой БД теряет оплату НАВСЕГДА: провайдер считает вебхук
	// доставленным и больше не придёт.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "payment: operation could not be completed")
)
