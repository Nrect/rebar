package auth

import "github.com/google/uuid"

// Principal — кто пришёл с запросом: результат разбора сессии.
type Principal struct {
	Realm     Realm
	SubjectID uuid.UUID
	// SessionHash — HMAC сессионного токена, а не сам токен: значение попадает
	// в контекст запроса и в аудит потребителя, а сырой токен не должен
	// оказаться ни там, ни там (doc.go, «Безопасность», п. 3).
	SessionHash string
}

// IsZero — пустой ли принципал (запрос без сессии).
func (p Principal) IsZero() bool {
	return p.Realm == "" && p.SubjectID == uuid.Nil && p.SessionHash == ""
}

// Identity — строка таблицы пользователей потребителя в том объёме, который
// нужен входу. Профиль, связи и внешние ключи остаются у потребителя: своей
// таблицы пользователей у пакета нет и не будет.
type Identity struct {
	ID    uuid.UUID
	Login string
	// PasswordHash — PHC-строка argon2id (auth/password). Секрет: в лог, в
	// текст ошибки и в метку метрики не попадает.
	PasswordHash string
	// Verified — подтверждён ли адрес.
	Verified bool
	// Disabled — отключён ли субъект. Отключённый отвергается сразу, не
	// дожидаясь истечения сессии.
	Disabled bool
}
