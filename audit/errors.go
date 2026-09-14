package audit

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; класс для HTTP-статуса
// несёт сама sentinel (ADR-0007). Значений подробностей в текстах нет — только
// имена ключей, которые пишет его же код. Префикс «audit:» в тексте обязателен:
// KindError равны по классу и тексту, и без него audit.ErrUnavailable совпала
// бы через errors.Is с ErrUnavailable другого модуля того же текста.
var (
	// ErrUnknownAction — действия нет в Config.Actions. Набор закрыт, чтобы
	// опечатка не заводила новый код действия навсегда.
	//errs:nokind действие выбирает код потребителя из своего реестра: необъявленное — дефект сборки, то есть 500
	ErrUnknownAction = errors.New("audit: action is not declared in Config.Actions")
	// ErrInvalidEntry — исход или род актора вне закрытого набора.
	//errs:nokind исход ставит код потребителя, род актора — его обвязка входа: вне набора — дефект вызывающего, то есть 500
	ErrInvalidEntry = errors.New("audit: entry is invalid")
	// ErrForbiddenDetail — ключ подробности похож на секрет (пароль, токен,
	// секрет, заголовок авторизации, кука).
	//errs:nokind ключ подробности пишет код потребителя, а не клиент: похожий на секрет — дефект вызывающего, то есть 500
	ErrForbiddenDetail = errors.New("audit: detail key looks like a secret")
	// ErrInvalidDetail — ключ пуст, длинен или непечатен либо подробностей
	// больше Config.MaxDetails.
	//errs:nokind ключи и число подробностей задаёт код потребителя, а значения из запроса усекаются, а не отвергаются: дефект вызывающего, то есть 500
	ErrInvalidDetail = errors.New("audit: detail is invalid")
	// ErrNoActor — в контексте нет актора: обвязка входа его не положила.
	//errs:nokind актора кладёт обвязка входа потребителя, а аноним ставится явно: не положила — дефект сборки, то есть 500, а не 401
	ErrNoActor = errors.New("audit: actor is missing from context")
	// ErrUnavailable — приёмник не смог записать событие. Падать или
	// продолжать, решает вызывающий: это его политика, не наша. Наружу 503:
	// повтор осмыслен.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "audit: event could not be recorded")
)
