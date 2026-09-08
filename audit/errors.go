package audit

import "errors"

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; значений подробностей
// в текстах нет — только имена ключей, которые пишет его же код.
var (
	// ErrUnknownAction — действия нет в Config.Actions. Набор закрыт, чтобы
	// опечатка не заводила новый код действия навсегда.
	ErrUnknownAction = errors.New("audit action is not declared in Config.Actions")
	// ErrInvalidEntry — исход или род актора вне закрытого набора.
	ErrInvalidEntry = errors.New("audit entry is invalid")
	// ErrForbiddenDetail — ключ подробности похож на секрет (пароль, токен,
	// секрет, заголовок авторизации, кука).
	ErrForbiddenDetail = errors.New("audit detail key looks like a secret")
	// ErrInvalidDetail — ключ пуст, длинен или непечатен либо подробностей
	// больше Config.MaxDetails.
	ErrInvalidDetail = errors.New("audit detail is invalid")
	// ErrNoActor — в контексте нет актора: обвязка входа его не положила.
	ErrNoActor = errors.New("audit actor is missing from context")
	// ErrUnavailable — приёмник не смог записать событие. Падать или
	// продолжать, решает вызывающий: это его политика, не наша.
	ErrUnavailable = errors.New("audit event could not be recorded")
)
