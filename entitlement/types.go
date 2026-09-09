package entitlement

import "time"

// MaxItemIDLen — потолок длины идентификатора предмета в байтах. Предмет
// живёт в каталоге потребителя, поэтому форма его идентификатора свободна;
// ограничена только длина, чтобы строка из внешнего мира не росла без предела.
const MaxItemIDLen = 128

// Grant — выдача: субъекту открыт предмет до указанного момента.
//
// ExpiresAt == nil — БЕССРОЧНО. Именно nil, а не нулевое время: нулевое
// time.Time лежит в прошлом, и опечатка «забыл заполнить» превратилась бы в
// вечный доступ либо в вечный отказ в зависимости от знака сравнения.
type Grant struct {
	// ItemID — предмет каталога потребителя: урок, курс, тариф.
	ItemID string
	// ExpiresAt — момент, С КОТОРОГО доступа уже нет (полуинтервал открыт
	// справа: at == ExpiresAt — закрыто). Та же граница обязана стоять в
	// адаптере: expires_at > $now.
	ExpiresAt *time.Time
}

// Open — открыта ли выдача в момент at.
func (g Grant) Open(at time.Time) bool {
	return g.ExpiresAt == nil || at.Before(*g.ExpiresAt)
}

// Reason — почему решение такое. ЗАКРЫТЫЙ НАБОР, спроектированный как метка
// метрики: по нему видно, отказ это правило, истёкший срок или сбой, и в нём
// нет ни субъекта, ни предмета — иначе кардинальность метрики задавал бы
// внешний мир (CONVENTIONS §2).
type Reason string

const (
	// ReasonAllow — предмет открыт: выдача есть и срок не вышел.
	ReasonAllow Reason = "allow"
	// ReasonNoGrant — записи о выдаче нет: запрет по умолчанию.
	ReasonNoGrant Reason = "deny_no_grant"
	// ReasonExpired — выдача есть, но срок вышел. Отдельно от no_grant:
	// «купил и кончилось» и «не покупал» — разные разговоры с клиентом.
	ReasonExpired Reason = "deny_expired"
	// ReasonError — решение не принято: хранилище не ответило либо сервис не
	// собран. Отдельно от отказов, чтобы алерт на сбой не тонул в штатных 403.
	ReasonError Reason = "error"
)

// AllReasons — полный набор; держит guard-тест.
var AllReasons = []Reason{
	ReasonAllow,
	ReasonNoGrant,
	ReasonExpired,
	ReasonError,
}

// Decision — исход проверки. Allowed — единственное, на чём ветвится код;
// Reason годится меткой метрики и строкой аудита.
//
// НУЛЕВОЕ ЗНАЧЕНИЕ — ОТКАЗ: забытое присваивание не открывает доступ.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// allow, deny — конструкторы решения: исход и причина не расходятся.
func allow() Decision        { return Decision{Allowed: true, Reason: ReasonAllow} }
func deny(r Reason) Decision { return Decision{Reason: r} }

// validItemID — непустой идентификатор в пределах потолка. Пустой запрещён
// отдельно: выдача с пустым предметом вела бы себя как шаблон «всё открыто».
func validItemID(itemID string) bool {
	return itemID != "" && len(itemID) <= MaxItemIDLen
}
