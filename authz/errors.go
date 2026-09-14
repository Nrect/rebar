package authz

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; ни субъекта, ни
// ресурса они не содержат. Класс для HTTP-статуса несёт сама sentinel
// (ADR-0007). Префикс «authz:» в тексте обязателен: KindError равны по классу и
// тексту, а у authz.ErrUnavailable и entitlement.ErrUnavailable текст после
// префикса дословно один.
var (
	// ErrUnavailable — решение не принято: источник ролей или хук политики
	// не ответили. У потребителя это 503, НИКОГДА не 403: «у вас нет прав»
	// во время недоступной базы — ложь клиенту и утопленный инцидент
	// (docs/CORRECTNESS.md, закон 9).
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "authz: decision could not be made")

	// ErrUnknownPermission — проверяется разрешение, не объявленное в
	// Config.Permissions. Это ошибка программиста (опечатка в константе), а
	// не отказ: молчаливое «нет» пряталось бы среди законных 403 до тех пор,
	// пока кто-нибудь не пожалуется на пропавший доступ.
	//errs:nokind разрешение называет константой код потребителя, а не клиент: опечатка — ошибка программиста, то есть 500
	ErrUnknownPermission = errors.New("authz: permission is not declared in Config")

	// ErrDenied — решение принято и оно отрицательное: 403. Заведён по образцу
	// entitlement.ErrDenied: два пакета — две оси одного механизма (ADR-0003),
	// и сообщать отказ они обязаны одинаково, иначе единой таблице
	// «ошибка → HTTP» (kit/errs/httperr) один из них не по зубам.
	//
	// Причина (Decision.Reason) приписывается к тексту: разбор жалобы иначе
	// требует логов, которых нет. НИ СУБЪЕКТА, НИ РЕСУРСА В ТЕКСТЕ НЕТ — они
	// данные, а текст ошибки доезжает до лога и до чужих глаз.
	ErrDenied = errs.Kinded(errs.KindForbidden, "authz: subject is not allowed to do this")
)
