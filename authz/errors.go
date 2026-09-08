package authz

import "errors"

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; ни субъекта, ни
// ресурса они не содержат.
var (
	// ErrUnavailable — решение не принято: источник ролей или хук политики
	// не ответили. У потребителя это 503, НИКОГДА не 403: «у вас нет прав»
	// во время недоступной базы — ложь клиенту и утопленный инцидент
	// (docs/CORRECTNESS.md, закон 9).
	ErrUnavailable = errors.New("authz: decision could not be made")

	// ErrUnknownPermission — проверяется разрешение, не объявленное в
	// Config.Permissions. Это ошибка программиста (опечатка в константе), а
	// не отказ: молчаливое «нет» пряталось бы среди законных 403 до тех пор,
	// пока кто-нибудь не пожалуется на пропавший доступ.
	ErrUnknownPermission = errors.New("authz: permission is not declared in Config")
)
