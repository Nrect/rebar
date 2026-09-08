package payment

import (
	"time"

	"github.com/google/uuid"
)

// Ограничения формы значений, попадающих в БД, в ключи дедупа и в метки метрик.
const (
	// MaxProviderNameLen — потолок имени провайдера.
	MaxProviderNameLen = 32
	// MaxReferenceLen — потолок ссылки потребителя.
	MaxReferenceLen = 128
	// MaxMethodLen — потолок способа оплаты.
	MaxMethodLen = 32
)

// ProviderName — имя платёжного провайдера, форма [a-z0-9_]{1,32}.
//
// НАБОР ЗАКРЫВАЕТ ПОТРЕБИТЕЛЬ, а не пакет: имя проверяется по форме, как Kind
// в mail. Закрытый enum здесь означал бы правку тулкита на каждого нового
// провайдера у каждого потребителя.
//
// Провайдер — часть ключа дедупа (provider, provider_event_id), а не только
// метка. Причина: id событий уникальны лишь в пространстве одного провайдера.
// Дедуп без провайдера означал бы, что при переезде на второго его событие с
// совпавшим id молча проглотили бы как дубль первого — то есть потеряли бы
// оплату.
type ProviderName string

func (p ProviderName) valid() bool {
	return matchesForm(string(p), MaxProviderNameLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
	})
}

// Method — способ оплаты, форма [a-z_]{1,32} ("bank_card", "sbp", …).
//
// Пустое значение законно и означает «способ выбирает плательщик на стороне
// провайдера» — самый частый сценарий редиректа на платёжную страницу.
// Непустое обязано быть в Config.Methods: набор закрывает потребитель, пакет
// проверяет только форму и членство.
type Method string

func (m Method) valid() bool {
	return matchesForm(string(m), MaxMethodLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c == '_'
	})
}

// ConfirmationType — как плательщик подтверждает платёж. Закрытый набор:
// значение попадает в БД и в метку метрики.
type ConfirmationType string

const (
	// ConfirmationRedirect — уводим браузер на страницу провайдера.
	ConfirmationRedirect ConfirmationType = "redirect"
	// ConfirmationQR — показываем QR (СБП): платят другим устройством.
	ConfirmationQR ConfirmationType = "qr"
	// ConfirmationEmbedded — виджет провайдера внутри нашей страницы.
	ConfirmationEmbedded ConfirmationType = "embedded"
)

// AllConfirmationTypes — полный список; держит guard-тест.
var AllConfirmationTypes = []ConfirmationType{
	ConfirmationRedirect, ConfirmationQR, ConfirmationEmbedded,
}

func (t ConfirmationType) valid() bool {
	switch t {
	case ConfirmationRedirect, ConfirmationQR, ConfirmationEmbedded:
		return true
	default:
		return false
	}
}

// Confirmation — чем плательщик подтверждает платёж.
//
// Не голая ссылка: у СБП подтверждение — payload QR, у виджета — токен, и
// «ConfirmationURL string» заставлял бы адаптер класть в поле «ссылка» то, что
// ссылкой не является. Тип говорит вызывающему, что рисовать.
type Confirmation struct {
	Type ConfirmationType
	// URL — куда вести браузер (redirect) либо где лежит виджет (embedded).
	URL string
	// QRPayload — строка для отрисовки QR. Секретом не является, но и в лог не
	// идёт: по ней платят.
	QRPayload string
}

// Intent — одна попытка оплаты: кто, за что, на какую сумму и чем платит.
//
// AmountMinor, Currency и Items — СНАПШОТ на момент создания, а не ссылка на
// каталог потребителя. Изменение цены между «показали кнопку» и «пришло
// событие» не должно менять сделку, которую человек уже подтвердил. Ровно
// поэтому же сверка сумм сравнивает с этими полями, а не с текущим прайсом.
type Intent struct {
	ID uuid.UUID
	// PayerID — плательщик: пользователь либо гостевая сессия. Пакет о нём
	// знает ровно то, что он uuid: FK на таблицу потребителя схема не ставит.
	PayerID uuid.UUID
	// Reference — id заказа (или иной сущности) у потребителя, форма
	// [A-Za-z0-9:_-]{1,128}.
	//
	// На него опирается правило «одно живое намерение на Reference»
	// (Store.CreateIntent): второй платёж за тот же заказ означал бы два
	// списания за одну покупку.
	Reference string

	// AmountMinor — итог: Σ сумм позиций. Именно он списывается и именно с ним
	// сверяется сумма события провайдера.
	AmountMinor int64
	Currency    string
	// Items — состав расчёта, позиции 0..n-1 (см. CheckItems). Хранится вместе
	// с намерением и возвращается на КАЖДОМ чтении: состав, заполненный
	// «иногда», однажды окажется пустым там, где по нему собирают чек.
	Items []OrderItem

	Provider ProviderName
	// Method — способ оплаты; пусто, если выбирает плательщик у провайдера.
	Method Method
	// AutoCapture — одностадийная оплата: провайдер списывает сразу, без холда.
	//
	// Хранится, а не выводится из запроса, потому что дозавершение брошенной
	// попытки (сверка) зовёт CreatePayment без исходного запроса: умолчание
	// адаптера молча превратило бы холд в списание или наоборот.
	AutoCapture bool
	// ProviderPaymentID и Confirmation пусты, пока статус created: платежа у
	// провайдера ещё нет.
	ProviderPaymentID string
	Confirmation      Confirmation

	Status Status

	// IdempotencyKey — УЖЕ нормализованный (NormalizeKey). Ни адаптер, ни стор
	// его не трогают: вторая точка нормализации — это второй набор правил.
	IdempotencyKey string
	// ParamsFingerprint — sha256 параметров покупки, 32 байта. Адаптер обязан
	// хранить и возвращать его байт в байт (контракт Store.CreateIntent и
	// Store.IntentByKey). Пустая сигнатура НЕ амнистируется: иначе потерянная
	// колонка тихо превратила бы чужую покупку под тем же ключом в законный
	// повтор и отдала бы клиенту чужую ссылку на оплату.
	ParamsFingerprint []byte

	CreatedAt time.Time
	UpdatedAt time.Time
	// ExpiresAt — CreatedAt + Config.IntentTTL. Не оптимизация, а срок годности
	// предложения: ссылка с зафиксированной ценой не должна пережить смену цены
	// в каталоге.
	ExpiresAt time.Time
	// SettledAt непусто тогда и только тогда, когда статус succeeded.
	SettledAt *time.Time
}

// Money — сумма намерения как значение: сравнения и суммирование идут через
// него, чтобы валюта не потерялась по дороге.
func (i Intent) Money() (Money, error) { return NewMoney(i.AmountMinor, i.Currency) }

// LedgerKind — род денежной записи.
type LedgerKind string

const (
	// LedgerCapture — деньги пришли. Одна такая запись на намерение: держит
	// частичный уникальный индекс адаптера.
	LedgerCapture LedgerKind = "capture"
	// LedgerRefund — компенсирующая запись: деньги вернули. Их может быть
	// несколько (частичные возвраты), суммарно не больше зачисления.
	LedgerRefund LedgerKind = "refund"
)

// AllLedgerKinds — полный список; зеркалит CHECK схемы.
var AllLedgerKinds = []LedgerKind{LedgerCapture, LedgerRefund}

// LedgerEntry — неизменяемая денежная запись.
//
// Не мутируется и не удаляется НИКОГДА: отмена это новая встречная запись со
// ссылкой ReversesEntryID. Мутируемое поле «оплачено» плюс конкурентные запросы —
// классика двоения денег; книга делает дубль и потерю видимыми и исправимыми,
// мутация — невидимыми и вечными.
//
// AmountMinor всегда положительна: род записи несёт Kind, а не знак суммы.
// Отрицательные суммы сделали бы «сколько всего получено» вопросом с двумя
// разными ответами в зависимости от того, кто как просуммировал.
type LedgerEntry struct {
	ID          uuid.UUID
	IntentID    uuid.UUID
	Kind        LedgerKind
	AmountMinor int64
	Currency    string
	// ProviderEventID — событие, породившее запись: по нему разбирают с
	// провайдером, откуда взялись деньги.
	ProviderEventID string
	// ReversesEntryID непусто ровно у refund и указывает на зачисление.
	// Уникальным НЕ является: частичных возвратов на одно зачисление бывает
	// несколько, а их потолок — не число записей, а Σrefund ≤ capture.
	ReversesEntryID *uuid.UUID
	// IdempotencyKey — производный и детерминированный (см. idempotency.go):
	// два прохода одной операции дают один ключ, и запись не удвоится даже
	// мимо механизма дедупа. Уникален в пределах намерения (UNIQUE в адаптере).
	IdempotencyKey string
	// ActorID — КТО подвинул деньги. Непусто у ручных операций (возврат), nil у
	// автоматических (зачисление по событию провайдера), где автор — само
	// событие и оно названо в ProviderEventID.
	//
	// Поле заведено до первой миграции намеренно: денежная строка без автора
	// оставляет вопрос «кто вернул эти деньги» без ответа НАВСЕГДА — дописать
	// автора в уже записанные строки будет нечем.
	ActorID   *uuid.UUID
	CreatedAt time.Time
}

// Money — сумма записи как значение.
func (e LedgerEntry) Money() (Money, error) { return NewMoney(e.AmountMinor, e.Currency) }

// EventType — что провайдер сообщает. Закрытый набор; всё, что адаптер не смог
// отобразить сюда, получает EventIgnored. «Похоже на succeeded» здесь
// запрещено: домысленный успех — это отданный бесплатно товар.
type EventType string

const (
	EventSucceeded EventType = "succeeded"
	// EventAuthorized — холд поставлен, деньги заморожены, но не списаны.
	EventAuthorized EventType = "authorized"
	EventCanceled   EventType = "canceled"
	EventFailed     EventType = "failed"
	EventRefunded   EventType = "refunded"
	EventPending    EventType = "pending"
	EventIgnored    EventType = "ignored"
)

// AllEventTypes — полный список; держит guard-тест.
var AllEventTypes = []EventType{
	EventSucceeded, EventAuthorized, EventCanceled,
	EventFailed, EventRefunded, EventPending, EventIgnored,
}

func (t EventType) valid() bool {
	switch t {
	case EventSucceeded, EventAuthorized, EventCanceled,
		EventFailed, EventRefunded, EventPending, EventIgnored:
		return true
	default:
		return false
	}
}

// Event — нормализованное событие провайдера. Адаптер приводит к этому виду
// ЛЮБОЙ формат: пакет не знает ни JSON провайдера, ни его словаря статусов, ни
// его HTTP-схемы.
type Event struct {
	Provider ProviderName
	// ProviderEventID — ключ дедупа. Пустой это ErrMalformedEvent: событие без
	// собственного id невозможно дедуплицировать, а значит его повторная
	// доставка зачислила бы деньги дважды.
	//
	// Адаптер обязан выводить его ДЕТЕРМИНИРОВАННО из состояния платежа
	// (например "<provider>:<paymentID>:<status>"), чтобы вебхук и сверка
	// дедуплицировались друг с другом, а не применяли одно событие дважды.
	ProviderEventID   string
	ProviderPaymentID string
	// IntentID — из metadata провайдера. uuid.Nil означает орфана: событие
	// записывается, но применить его не к чему.
	IntentID    uuid.UUID
	Type        EventType
	AmountMinor int64
	Currency    string
	OccurredAt  time.Time
}

// Money — сумма события как значение.
func (e Event) Money() (Money, error) { return NewMoney(e.AmountMinor, e.Currency) }

// DriftKind — род расхождения книг. Закрытый набор: метка gauge payment_drift.
type DriftKind string

const (
	// DriftSucceededNoCapture — намерение оплачено, а денежной записи нет.
	DriftSucceededNoCapture DriftKind = "succeeded_no_capture"
	// DriftCaptureNotSucceeded — деньги в книге, а намерение не succeeded:
	// зачисление проехало, статус нет.
	DriftCaptureNotSucceeded DriftKind = "capture_not_succeeded"
	// DriftRefundOverCapture — возвращено больше полученного. Триггер книги
	// такого не допускает, значит строки правили мимо приложения.
	DriftRefundOverCapture DriftKind = "refund_over_capture"
)

// AllDriftKinds — полный список; держит guard-тест.
var AllDriftKinds = []DriftKind{
	DriftSucceededNoCapture, DriftCaptureNotSucceeded, DriftRefundOverCapture,
}

// DriftRecord — одно расхождение. Ноль расхождений — это утверждение, которое
// должно проверяться, а не подразумеваться.
type DriftRecord struct {
	IntentID  uuid.UUID
	PayerID   uuid.UUID
	Reference string
	Kind      DriftKind
}

// IntentCursor — позиция в очереди сверки: пара (created_at, id) последнего
// РАССМОТРЕННОГО намерения. Нулевое значение означает «с головы очереди».
//
// ОТКАЗ, который курсор предотвращает: ГОЛОВА ОЧЕРЕДИ КАК АБСОРБИРУЮЩЕЕ
// СОСТОЯНИЕ. Очередь отсортирована по created_at и обрезана размером пачки, а
// исходы provider_error и still_pending статус намерения НЕ МЕНЯЮТ — строка
// остаётся в очереди на том же месте и на следующем прогоне снова оказывается
// в той же пачке. Без курсора достаточно, чтобы впереди стояла пачка намерений,
// о которых провайдер отвечает ошибкой, — или просто чтобы очередь была
// длиннее, чем успевает разобрать бюджет прогона, — и хвост не спрашивают
// НИКОГДА. А в хвосте лежит самое свежее намерение: тот, кто заплатил только
// что и чей вебхук потерялся.
//
// Курсор двигается по (created_at, id) — ровно по тому же ключу, по которому
// упорядочена очередь: новые зависшие намерения всегда СВЕЖЕЕ курсора, то есть
// попадают в текущий круг, а не ждут следующего.
type IntentCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// matchesForm — строка непуста, не длиннее maxLen байт и состоит только из
// разрешённых байтов.
//
// Проверка побайтовая, а не по рунам: все три формы пакета — ASCII, а значение
// уезжает в колонку БД, в ключ дедупа и в метку метрики, где многобайтная руна
// не нужна никому и однажды приедет из внешнего мира.
func matchesForm(s string, maxLen int, allowed func(byte) bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if !allowed(s[i]) {
			return false
		}
	}
	return true
}

// validReference — форма ссылки потребителя: [A-Za-z0-9:_-]{1,128}.
//
// Двоеточие разрешено намеренно: составные ссылки вида "order:42" — самая
// частая форма у потребителя, а пробел, слэш и кавычка запрещены, чтобы
// значение не пришлось экранировать ни в логе, ни в URL, ни в metadata
// провайдера.
func validReference(s string) bool {
	return matchesForm(s, MaxReferenceLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == ':' || c == '_' || c == '-'
	})
}
