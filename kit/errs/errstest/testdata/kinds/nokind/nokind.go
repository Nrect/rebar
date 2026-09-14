package nokind

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/kit/errs"
)

// ErrRefused — класса нет намеренно, и довод назван: не находка.
//
//errs:nokind класс зависит от вызывающего
var ErrRefused = errors.New("nokind: refused")

//errs:nokind
var ErrNoReason = errors.New("nokind: no reason")

//errs:nokind класс не нужен
var ErrBothWays = errs.Kinded(errs.KindConflict, "nokind: both ways")

var (
	//errs:nokind в блоке директива стоит над своим объявлением
	ErrRefusedInBlock = errors.New("nokind: refused in block")

	// Обёртке директива не нужна: класс берётся у обёрнутой.
	//errs:nokind обёртка
	ErrWrappedRefused = fmt.Errorf("%w: wrapped", ErrBothWays)
)

//errs:nokind над блоком директива не прячет его sentinel'ы
var (
	ErrUnderBlockDirective = errors.New("nokind: under block directive")
)

// errs:nokind с пробелом после // — обычный комментарий, не директива
var ErrSpaced = errors.New("nokind: spaced")
