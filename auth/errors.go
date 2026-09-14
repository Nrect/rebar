package auth

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки корня. Вызывающий ветвится через errors.Is; ни логина, ни
// хэша, ни токена в текстах нет. Класс для HTTP-статуса несёт сама sentinel
// (ADR-0007). Префикс «auth:» в тексте обязателен: KindError равны по классу и
// тексту, и без него sentinel разных модулей совпадали бы через errors.Is.
var (
	// ErrInvalidRealm — реалм не соответствует форме [a-z0-9_]{1,32}.
	//errs:nokind реалм задаёт конфигурация сборки, а не запрос: негодный — дефект сборки, то есть 500
	ErrInvalidRealm = errors.New("auth: realm is invalid")
	// ErrIdentityNotFound — личности с таким логином или идентификатором нет.
	// Отсутствие строки, а не сбой: вход отвечает на неё тем же, чем на
	// неверный пароль.
	//errs:nokind сигнал порта Identities: session сворачивает её на всех путях, а готовый 404 на входе или сбросе — перебор адресов
	ErrIdentityNotFound = errors.New("auth: identity not found")
	// ErrLoginTaken — логин занят. Регистрация её наружу не отдаёт: отвечает
	// «принято» и шлёт письмо владельцу адреса, иначе форма регистрации
	// становится проверялкой существования адреса. ЕДИНСТВЕННЫЙ ЗАКОННЫЙ
	// ВЫХОД — session.ConfirmEmailChange: ссылку открыл владелец нового
	// адреса, и «занят» он узнал бы и восстановлением пароля на нём же.
	// Класс 409: состояние логина не допускает смену.
	ErrLoginTaken = errs.Kinded(errs.KindConflict, "auth: login is already taken")
	// ErrUnavailable — сбой источника личностей. Даёт 503, а не 401: «неверные
	// данные» при недоступной базе учит поддержку неверному диагнозу.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "auth: identity store is unavailable")
)
