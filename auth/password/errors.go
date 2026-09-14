package password

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Пароля, хэша и логина в текстах нет ни в одном случае.
// Класс для HTTP-статуса несёт сама sentinel (ADR-0007).
var (
	// ErrBusy — потолок одновременных хеширований занят.
	//
	// ОТДЕЛЬНАЯ ОШИБКА, И ОНА НЕ ОБОРАЧИВАЕТ КОНТЕКСТ. Обёртка сделала бы
	// errors.Is(err, context.DeadlineExceeded) истинным, и слой HTTP отдал бы
	// таймаут вместо отказа по занятости: разные алерт и инцидент. У неё же
	// нет ни Permanent(), ни RetryAfter() — вызывающий обязан свернуть её в
	// обычный отказ входа, а Retry-After сообщил бы перебирающему, что он
	// нашёл потолок. Класс 429 заголовка не добавляет: httperr берёт его
	// только из RetryAfter().
	ErrBusy = errs.Kinded(errs.KindTooManyRequests, "password: password hashing is at capacity")
	// ErrHashInvalid — строка не разбирается как argon2id PHC или её
	// параметры вне потолков. Дефект данных, а не неверный пароль.
	//errs:nokind строку хэша пишет хранилище потребителя, а не клиент: битая колонка — инцидент, то есть 500
	ErrHashInvalid = errors.New("password: password hash is malformed or out of bounds")

	// ErrTooShort, ErrTooLong, ErrTooWeak — причины отказа Policy. Класс у
	// всех трёх один, 400: подробность «слишком слабый» полезна владельцу
	// пароля и бесполезна атакующему, но три разных статуса на форме
	// регистрации — это три разных сигнала. Различать ли причины слагом,
	// решает словарь потребителя (ADR-0007).
	ErrTooShort = errs.Kinded(errs.KindIncorrectInput, "password: password is shorter than the minimum length")
	ErrTooLong  = errs.Kinded(errs.KindIncorrectInput, "password: password exceeds the maximum length")
	ErrTooWeak  = errs.Kinded(errs.KindIncorrectInput, "password: password is too easy to guess")
)
