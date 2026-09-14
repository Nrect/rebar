package bad

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/kit/errs"
)

// ErrPlain — без класса: так объявлены sentinel'ы до ADR-0007.
var ErrPlain = errors.New("bad: plain")

var (
	ErrFormatted  = fmt.Errorf("bad: formatted")
	ErrPercent    = fmt.Errorf("bad: 100%%w is not a wrap")
	ErrFormatVar  = fmt.Errorf(format)
	ErrUnknown    = errs.Kinded(errs.KindUnknown, "bad: unknown")
	ErrNewUnknown = errs.New(errs.KindUnknown, "bad-new-unknown")
	ErrSlugless   = errs.Unknown("bad-unknown")
	ErrHandBuilt  = errs.KindError{}
	ErrZero       errs.KindError
	ErrNoValue    error
	ErrOpaque     = newError("bad: opaque")
)

var ErrFirst, ErrSecond = errors.New("bad: first"), errs.Kinded(errs.KindConflict, "bad: second")

var ErrPairA, ErrPairB = pair()

// Имя не Err…, но значение — ошибка без класса.
var NotNamedErr = errors.New("bad: not named")

// Не находки: обёртка, неэкспортируемая, не ошибка.
var (
	ErrWrapped   = fmt.Errorf("%w: wrapped", ErrPlain)
	errInternal  = errors.New("bad: internal")
	DefaultLimit = 10
	Errand       = 42
)

const format = "bad: %s"

func newError(msg string) error { return errors.New(msg) }

func pair() (error, error) { return nil, nil }

// Локальная переменная — не sentinel пакета.
func local() error {
	var ErrLocal = errors.New("bad: local")
	return ErrLocal
}
