package session

// NotificationKind — что за письмо. Закрытый набор: значение уезжает в шаблон
// потребителя и в метку его метрики, поэтому новое значение — минорное
// изменение, а переименование и удаление — ломающее (CONVENTIONS §10).
type NotificationKind string

const (
	// NotifyVerify — ссылка подтверждения адреса после регистрации.
	NotifyVerify NotificationKind = "verify"
	// NotifyReset — ссылка сброса пароля.
	NotifyReset NotificationKind = "reset"
	// NotifyEmailChange — ссылка подтверждения НОВОГО адреса.
	NotifyEmailChange NotificationKind = "email_change"
	// NotifyLoginTaken — письмо владельцу адреса, на который попытались
	// зарегистрироваться повторно. ИМЕННО ОНО ДЕЛАЕТ ОТВЕТ «ПРИНЯТО»
	// ЧЕСТНЫМ: форма регистрации не говорит «адрес занят», а владелец адреса
	// узнаёт о попытке — и о том, что аккаунт у него уже есть.
	NotifyLoginTaken NotificationKind = "login_taken"
	// NotifyPasswordChanged — пароль сменили. Уходит после смены, чтобы
	// владелец увидел чужую смену раньше, чем потеряет доступ.
	NotifyPasswordChanged NotificationKind = "password_changed"
	// NotifyLoginChangeRequested — предупреждение на ПРЕЖНИЙ адрес о заказанной
	// смене логина. Ссылка уходит на новый адрес, предупреждение — на старый:
	// иначе увод аккаунта проходит молча для того, у кого его уводят.
	NotifyLoginChangeRequested NotificationKind = "login_change_requested"
)

// AllNotificationKinds — полный набор; держит guard-тест.
var AllNotificationKinds = []NotificationKind{
	NotifyVerify, NotifyReset, NotifyEmailChange,
	NotifyLoginTaken, NotifyPasswordChanged, NotifyLoginChangeRequested,
}

// Valid — известное ли это письмо. Неизвестное — отказ: шаблона под него у
// потребителя нет, и письмо ушло бы пустым.
func (k NotificationKind) Valid() bool { return validKind(k, AllNotificationKinds) }

// String — вид письма строкой; персональных данных в нём нет, метка метрики
// из него законна.
func (k NotificationKind) String() string { return string(k) }

// EventKind — что случилось. Закрытый набор ровно потому же, почему закрыт
// NotificationKind: значение уезжает в журнал потребителя и в метку метрики.
type EventKind string

const (
	// EventRegistered — личность заведена.
	EventRegistered EventKind = "registered"
	// EventSignedIn — вход состоялся.
	EventSignedIn EventKind = "signed_in"
	// EventSignInFailed — вход не состоялся. Причина в событие НЕ пишется:
	// журнал, различающий «нет логина» и «не тот пароль», — та же проверялка
	// существования, только доступная тому, кто читает журнал.
	EventSignInFailed EventKind = "sign_in_failed"
	// EventLockedOut — счётчик попыток исчерпан.
	EventLockedOut EventKind = "locked_out"
	// EventSignedOut — выход одной сессии.
	EventSignedOut EventKind = "signed_out"
	// EventSignedOutAll — выход со всех устройств.
	EventSignedOutAll EventKind = "signed_out_all"
	// EventPasswordChanged — пароль сменён; все сессии и токены сброса отозваны.
	EventPasswordChanged EventKind = "password_changed"
	// EventVerified — адрес подтверждён.
	EventVerified EventKind = "verified"
	// EventLoginChanged — логин сменён.
	EventLoginChanged EventKind = "login_changed"
	// EventTokenIssued — выдан одноразовый токен.
	EventTokenIssued EventKind = "token_issued"
)

// AllEventKinds — полный набор; держит guard-тест.
var AllEventKinds = []EventKind{
	EventRegistered, EventSignedIn, EventSignInFailed, EventLockedOut,
	EventSignedOut, EventSignedOutAll, EventPasswordChanged, EventVerified,
	EventLoginChanged, EventTokenIssued,
}

// Valid — известное ли это событие.
func (k EventKind) Valid() bool { return validKind(k, AllEventKinds) }

// String — вид события строкой; законная метка метрики.
func (k EventKind) String() string { return string(k) }

func validKind[T comparable](k T, all []T) bool {
	for _, known := range all {
		if k == known {
			return true
		}
	}
	return false
}
