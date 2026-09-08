package authz

import (
	"strings"
	"unicode/utf8"
)

// Ограничения формы: имена ролей и разрешений живут в конфигурации
// потребителя и в его миграциях, операции — в его роутере.
const (
	// MaxNameLen — потолок длины роли и разрешения в байтах.
	MaxNameLen = 64
	// MaxOperationLen — потолок длины имени операции в байтах.
	MaxOperationLen = 128
	// MaxSubjectIDLen — потолок длины идентификатора субъекта в байтах.
	MaxSubjectIDLen = 128
	// MaxRealmLen — потолок длины реалма в байтах.
	MaxRealmLen = 32
)

// Subject — кто спрашивает. Realm разделяет несвязанные множества субъектов
// (покупатели и персонал — разные реалмы с разными таблицами); у потребителя
// с одним множеством он пустой.
//
// НУЛЕВОЕ ЗНАЧЕНИЕ — АНОНИМ, И ЭТО ОТКАЗ (deny_no_subject). Пустой ID не
// «любой субъект», а «субъекта нет»: аутентификацию делает auth, и её
// отсутствие не должно выглядеть как успешная проверка прав.
type Subject struct {
	Realm string
	ID    string
}

// Anonymous — субъекта нет: запрос без сессии либо сессия не разобрана.
func (s Subject) Anonymous() bool { return s.ID == "" }

// Role — роль субъекта. Закрытый набор задаёт Config.Roles; роль, пришедшая
// из источника и не объявленная в Config, разрешений не даёт.
type Role string

// Permission — право на класс действий. Закрытый набор — Config.Permissions.
type Permission string

// Operation — операция API потребителя: маршрут, метод gRPC, имя сценария.
// Ключ реестра; операции без правила отказывают (default deny).
type Operation string

// Resource — над чем действие, если оно предметно. Тип и идентификатор
// строками: пакет не знает доменных типов потребителя, а порт остаётся
// копируемым (CONVENTIONS §1). Нулевое значение — «ресурс не назван».
type Resource struct {
	Type string
	ID   string
}

// Zero — ресурс не назван.
func (r Resource) Zero() bool { return r.Type == "" && r.ID == "" }

// Reason — почему решение такое. ЗАКРЫТЫЙ НАБОР, спроектированный как метка
// метрики: по нему видно, отказ это правило или сбой, и в нём нет ни
// субъекта, ни ресурса — иначе кардинальность метрики задавал бы внешний мир
// (CONVENTIONS §2).
type Reason string

const (
	// ReasonAllow — разрешено: роль даёт разрешение и политика не сузила.
	ReasonAllow Reason = "allow"
	// ReasonNoSubject — субъекта нет (аноним).
	ReasonNoSubject Reason = "deny_no_subject"
	// ReasonNoRole — у субъекта нет ни одной роли, объявленной в Config.
	ReasonNoRole Reason = "deny_no_role"
	// ReasonNoPermission — роли есть, разрешения среди них нет.
	ReasonNoPermission Reason = "deny_no_permission"
	// ReasonPolicy — RBAC разрешил, хук политики сузил.
	ReasonPolicy Reason = "deny_policy"
	// ReasonUnclassified — операции нет в реестре: default deny.
	ReasonUnclassified Reason = "deny_unclassified"
	// ReasonError — решение не принято: сбой источника ролей или политики
	// либо неизвестное разрешение. Отдельно от отказов, чтобы алерт на сбой
	// не тонул в штатных 403.
	ReasonError Reason = "error"
)

// AllReasons — полный набор; держит guard-тест.
var AllReasons = []Reason{
	ReasonAllow,
	ReasonNoSubject,
	ReasonNoRole,
	ReasonNoPermission,
	ReasonPolicy,
	ReasonUnclassified,
	ReasonError,
}

// Decision — исход проверки. Allowed — единственное, на чём ветвится код;
// Reason годится меткой метрики и строкой аудита.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// allow, deny — конструкторы решения: исход и причина не расходятся.
func allow() Decision        { return Decision{Allowed: true, Reason: ReasonAllow} }
func deny(r Reason) Decision { return Decision{Reason: r} }

// validName — форма роли и разрешения: [a-z0-9_.:-]{1,MaxNameLen}. Нижний
// регистр закрывает целый класс расхождений «Admin» и «admin» в конфиге и в
// базе.
func validName(s string) bool {
	if s == "" || len(s) > MaxNameLen {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == ':' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// validOperation — имя операции: непустое, в пределах потолка, корректный
// UTF-8 без управляющих символов. Форма свободнее ролей: сюда пишут «GET
// /orders» и «OrderService/Create». Управляющие символы запрещены — они
// ломают текст ошибки и строку аудита, куда операция попадает как есть.
func validOperation(op string) bool {
	if op == "" || len(op) > MaxOperationLen || !utf8.ValidString(op) {
		return false
	}
	return !strings.ContainsFunc(op, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
