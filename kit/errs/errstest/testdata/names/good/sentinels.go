package good

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/kit/errs"
)

// Каждая экспортируемая sentinel называет свой пакет.
var (
	ErrClassed   = errs.Kinded(errs.KindConflict, "good: classed")
	ErrPlain     = errors.New("good: plain")
	ErrRaw       = errors.New(`good: raw literal`)
	ErrFormatted = fmt.Errorf("good: formatted without a wrap")
)

// Обёртка несёт текст обёрнутой, слаг — не текст, неэкспортируемой правило не
// нужно, экспортируемая не-ошибка не проверяется.
var (
	ErrWrapped    = fmt.Errorf("%w: good: with detail", ErrClassed)
	ErrSlugged    = errs.NotFound("good-not-found")
	ErrNewSlugged = errs.New(errs.KindUnavailable, "good-unavailable")
	errInternal   = errors.New("internal without a package")
	DefaultLimit  = 10
)
