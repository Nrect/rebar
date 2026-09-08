package audit

import (
	"time"

	"github.com/google/uuid"
)

// MaxActionLen — потолок длины Action.
const MaxActionLen = 64

// Action — действие журнала (user_login, order_refund). Набор объявляет
// потребитель в Config.Actions; синтаксис [a-z0-9_.]{1,64}, потому что это
// метка метрики.
type Action string

func (a Action) valid() bool {
	if a == "" || len(a) > MaxActionLen {
		return false
	}
	for _, r := range a {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// Outcome — чем кончилось действие (закрытый набор: метка метрики и колонка
// с CHECK).
type Outcome string

const (
	// OutcomeSuccess — действие выполнено.
	OutcomeSuccess Outcome = "success"
	// OutcomeDenied — отказ: система ответила определённо «нет» (нет прав,
	// неверный пароль).
	OutcomeDenied Outcome = "denied"
	// OutcomeFailure — сбой: действие не удалось не по решению домена либо
	// исход неизвестен.
	OutcomeFailure Outcome = "failure"
)

// AllOutcomes — полный список; держит guard-тест «CHECK ⊇ AllOutcomes».
var AllOutcomes = []Outcome{OutcomeSuccess, OutcomeDenied, OutcomeFailure}

// ОТКАЗ И СБОЙ РАЗДЕЛЕНЫ. Всплеск denied — подбор пароля или ошибка в правах,
// всплеск failure — авария; сведи их, и оба алерта будут гореть друг от друга.
func (o Outcome) valid() bool {
	return o == OutcomeSuccess || o == OutcomeDenied || o == OutcomeFailure
}

// ActorKind — род субъекта (закрытый набор: колонка с CHECK и первый столбец
// индекса по актору).
type ActorKind string

const (
	// ActorUser — человек, прошедший аутентификацию.
	ActorUser ActorKind = "user"
	// ActorService — другая система по своему ключу.
	ActorService ActorKind = "service"
	// ActorSystem — сам процесс: крон, обслуживание, миграция.
	ActorSystem ActorKind = "system"
	// ActorAnonymous — не аутентифицирован. Ставится обвязкой ЯВНО: «актора
	// в контексте нет» — это забытая обвязка, а не аноним (см. ErrNoActor).
	ActorAnonymous ActorKind = "anonymous"
)

// AllActorKinds — полный список; держит guard-тест «CHECK ⊇ AllActorKinds».
var AllActorKinds = []ActorKind{ActorUser, ActorService, ActorSystem, ActorAnonymous}

func (k ActorKind) valid() bool {
	return k == ActorUser || k == ActorService || k == ActorSystem || k == ActorAnonymous
}

// Actor — кто совершил действие. Кладётся в контекст обвязкой входа после
// аутентификации (NewContext) и берётся оттуда.
type Actor struct {
	Kind ActorKind
	// ID — идентификатор субъекта у потребителя; у ActorAnonymous пуст.
	ID string
	// Name — логин или отображаемое имя; в метку метрики не попадает.
	Name string
}

// Target — над чем действие. Набор типов не закрыт: в метки метрик они не
// идут, а перечислять чужие сущности в пакете нечем.
type Target struct {
	Type string
	ID   string
}

// Entry — действие так, как его описывает вызывающий. Актора здесь нет
// намеренно: он приходит из контекста (doc.go, п. 4).
type Entry struct {
	Action  Action
	Outcome Outcome
	// Target — необязателен: у входа в систему цели нет.
	Target Target
	// RequestID — идентификатор запроса из обвязки. Усекается: в HTTP он
	// приходит заголовком, то есть от клиента.
	RequestID string
	// IP — адрес клиента. Усекается по той же причине.
	IP string
	// Details — подробности строками. Ключи проверяются по списку
	// запрещённых, значения усекаются и чистятся.
	Details map[string]string
}

// Event — запись журнала: Entry после проверки действия и исхода, усечения
// враждебного ввода и подстановки актора, времени и идентификатора. Именно
// его получает Sink.
type Event struct {
	ID uuid.UUID
	// At — момент действия в UTC; назначает Recorder, а не база.
	At        time.Time
	Action    Action
	Outcome   Outcome
	Actor     Actor
	Target    Target
	RequestID string
	IP        string
	// Details — непустая карта (возможно, пустая по содержимому), не nil.
	Details map[string]string
}
