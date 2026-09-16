package bad

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/kit/errs"
)

// ErrNoPrefix — текст без имени пакета: так объявлены sentinel'ы до ADR-0007.
var ErrNoPrefix = errors.New("plain text without a package")

var (
	ErrOtherPackage = errs.Kinded(errs.KindConflict, "other: text of another package")
	ErrNoSpace      = errs.Kinded(errs.KindConflict, "bad:no space after the colon")
	ErrNotAtStart   = errs.Kinded(errs.KindConflict, "wrapped by bad: not at the start")
	ErrFormatted    = fmt.Errorf("formatted without a package")
	ErrRaw          = errors.New(`raw literal without a package`)
	ErrKindedConst  = errs.Kinded(errs.KindConflict, message)
	ErrNewConst     = errors.New(message)
)

var ErrFirst, ErrSecond = errors.New("first without a package"), errs.Kinded(errs.KindConflict, "bad: second")

// Имя не Err…, но значение — ошибка: правило то же.
var NotNamedErr = errors.New("not named and not prefixed")

// Отказ от класса префикс не снимает: он нужен ровно на тот день, когда
// sentinel класс получит.
//
//errs:nokind класс зависит от вызывающего
var ErrRefused = errors.New("refused and not prefixed")

// Не находки: обёртка (её Error() начинается с текста обёрнутой), конструкторы
// SlugError (Error() — сам слаг), нелитеральный формат Errorf (по нему не
// узнать даже, обёртка ли это), неузнанная форма, неэкспортируемая, не ошибка.
var (
	ErrWrapped   = fmt.Errorf("%w: bad: wrapped", ErrNoPrefix)
	ErrSlugged   = errs.NotFound("no-package-in-a-slug")
	ErrNewSlug   = errs.New(errs.KindUnavailable, "no-package-here-either")
	ErrFormatVar = fmt.Errorf(message)
	ErrOpaque    = newError("opaque without a package")
	errInternal  = errors.New("unexported without a package")
	DefaultLimit = 10
)

const message = "assembled elsewhere"

func newError(msg string) error { return errors.New(msg) }

// Локальная переменная — не sentinel пакета.
func local() error {
	var ErrLocal = errors.New("local without a package")
	return ErrLocal
}
