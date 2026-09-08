package password

import "errors"

// Sentinel-ошибки. Пароля, хэша и логина в текстах нет ни в одном случае.
var (
	// ErrBusy — потолок одновременных хеширований занят.
	//
	// ОТДЕЛЬНАЯ ОШИБКА, И ОНА НЕ ОБОРАЧИВАЕТ КОНТЕКСТ. Обёртка сделала бы
	// errors.Is(err, context.DeadlineExceeded) истинным, и слой HTTP отдал бы
	// таймаут вместо отказа по занятости: разные алерт и инцидент. У неё же
	// нет ни Permanent(), ни RetryAfter() — вызывающий обязан свернуть её в
	// обычный отказ входа, а Retry-After сообщил бы перебирающему, что он
	// нашёл потолок.
	ErrBusy = errors.New("password hashing is at capacity")
	// ErrHashInvalid — строка не разбирается как argon2id PHC или её
	// параметры вне потолков. Дефект данных, а не неверный пароль.
	ErrHashInvalid = errors.New("password hash is malformed or out of bounds")

	// ErrTooShort, ErrTooLong, ErrTooWeak — причины отказа Policy. Наружу
	// сворачиваются в одну: подробность «слишком слабый» полезна владельцу
	// пароля и бесполезна атакующему, но три разных статуса на форме
	// регистрации — это три разных сигнала.
	ErrTooShort = errors.New("password is shorter than the minimum length")
	ErrTooLong  = errors.New("password exceeds the maximum length")
	ErrTooWeak  = errors.New("password is too easy to guess")
)
