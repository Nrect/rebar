package auth

import "errors"

// Sentinel-ошибки корня. Вызывающий ветвится через errors.Is; ни логина, ни
// хэша, ни токена в текстах нет.
var (
	// ErrInvalidRealm — реалм не соответствует форме [a-z0-9_]{1,32}.
	ErrInvalidRealm = errors.New("realm is invalid")
	// ErrIdentityNotFound — личности с таким логином или идентификатором нет.
	// Отсутствие строки, а не сбой: вход отвечает на неё тем же, чем на
	// неверный пароль.
	ErrIdentityNotFound = errors.New("identity not found")
	// ErrLoginTaken — логин занят. Наружу не выходит: регистрация отвечает
	// «принято» и шлёт письмо владельцу адреса, иначе форма регистрации
	// становится проверялкой существования адреса.
	ErrLoginTaken = errors.New("login is already taken")
	// ErrUnavailable — сбой источника личностей. Даёт 503, а не 401: «неверные
	// данные» при недоступной базе учит поддержку неверному диагнозу.
	ErrUnavailable = errors.New("identity store is unavailable")
)
