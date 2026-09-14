package session

import "github.com/nrect/rebar/kit/errs"

// Sentinel-ошибки сервиса. Вызывающий ветвится через errors.Is; ни логина, ни
// пароля, ни сырого токена в текстах нет. Класс для HTTP-статуса несёт сама
// sentinel (ADR-0007).
//
// ОТВЕТЫ СХЛОПНУТЫ НАМЕРЕННО. «Нет такого логина», «не тот пароль», «хэш в
// колонке битый» и «субъект отключён» дают одну ErrInvalidCredentials: любое
// различие превращает форму входа в проверялку существования адреса, которая
// работает быстрее любого перебора паролей и не трогает счётчик блокировок.
var (
	// ErrInvalidCredentials — вход не состоялся. Один ответ на все причины.
	ErrInvalidCredentials = errs.Kinded(errs.KindUnauthenticated, "session: invalid credentials")
	// ErrTooManyAttempts — счётчик попыток по логину исчерпан ЛИБО потолок
	// одновременных хеширований занят. Два разных повода и один ответ:
	// отличимая перегрузка сама становится каналом перебора адресов
	// (ADR-0003, «Безопасность auth», п. 3).
	ErrTooManyAttempts = errs.Kinded(errs.KindTooManyRequests, "session: too many attempts")
	// ErrNotVerified — адрес не подтверждён, а AllowUnverifiedSignIn выключен.
	// Достижима только после ВЕРНОГО пароля, поэтому существование адреса ею
	// не выдаётся: тот, кто её увидел, и так знает пароль.
	ErrNotVerified = errs.Kinded(errs.KindForbidden, "session: identity is not verified")

	// ErrNoSession — сессии нет, она истекла либо субъект отключён. Одна
	// ошибка на все три случая: различать их значит рассказывать держателю
	// украденной куки, что именно с ней не так.
	ErrNoSession = errs.Kinded(errs.KindUnauthenticated, "session: no live session")
	// ErrTokenInvalid — одноразового токена нет, он погашен или истёк. Тоже
	// один ответ: «истёк» и «уже использован» вместе рассказали бы, что токен
	// был настоящим. Класс 400: токен приносит клиент ссылкой из письма.
	ErrTokenInvalid = errs.Kinded(errs.KindIncorrectInput, "session: one-time token is invalid")
)
