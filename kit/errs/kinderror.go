package errs

import "fmt"

// KindError — ошибка, знающая свой класс, но не слаг: класс объявляет пакет,
// слаг придумывает потребитель (ADR-0007).
//
// Значение, а не указатель, по образцу SlugError: sentinel из var-блока
// переиспользуется как цель errors.Is параллельными запросами.
type KindError struct {
	kind Kind
	msg  string
	// cause — исходная ошибка ДЛЯ ЛОГА. Наружу не уходят ни она, ни msg:
	// httperr отдаёт клиенту только имя класса.
	cause error
}

// Kinded — ошибка с классом и без слага. Паникует на Kind вне AllKinds и на
// KindUnknown: KindError существует ровно затем, чтобы нести класс, а «класс
// неизвестен» — это отсутствие класса, и такая ошибка объявляется errors.New.
// Объявляется в var-блоке, поэтому паника случится на старте, а не на запросе.
func Kinded(kind Kind, msg string) KindError {
	if !kind.valid() {
		panic(fmt.Sprintf("errs.Kinded: kind %q must be one of errs.AllKinds", kind))
	}
	if kind == KindUnknown {
		panic("errs.Kinded: kind must not be errs.KindUnknown (an error without a kind is errors.New)")
	}
	return KindError{kind: kind, msg: msg}
}

// Error — текст, а при наличии причины «текст: причина». Строка целиком идёт
// в лог; клиенту httperr отдаёт только имя класса.
func (e KindError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

// Kind — класс ошибки. Нулевое значение (var без Kinded) — KindUnknown: иначе
// guard модуля «класс не KindUnknown» пропустил бы забытую инициализацию.
func (e KindError) Kind() Kind {
	if e.kind == "" {
		return KindUnknown
	}
	return e.kind
}

// Unwrap — причина для errors.Is/As по цепочке.
func (e KindError) Unwrap() error { return e.cause }

// Is — равенство по классу и тексту; причина не участвует, как у SlugError.Is.
func (e KindError) Is(target error) bool {
	other, ok := target.(KindError)
	return ok && other.kind == e.kind && other.msg == e.msg
}

// WithCause — копия с причиной. Копия, а не мутация: цель errors.Is из
// var-блока переиспользуется параллельными запросами.
func (e KindError) WithCause(err error) KindError {
	e.cause = err
	return e
}
