package good

import (
	"errors"
	"fmt"

	"github.com/nrect/rebar/kit/errs"
)

// Каждая экспортируемая sentinel несёт класс или оборачивает ту, что несёт.
var (
	ErrClassed    = errs.Kinded(errs.KindConflict, "good: classed")
	ErrWrapped    = fmt.Errorf("%w: with detail", ErrClassed)
	ErrRawWrapped = fmt.Errorf(`%w: raw literal`, ErrClassed)
	ErrSlugged    = errs.NotFound("good-not-found")
	ErrNewSlugged = errs.New(errs.KindUnavailable, "good-unavailable")
)

// Неэкспортируемой класс не нужен; экспортируемая не-ошибка не проверяется.
var (
	errInternal  = errors.New("good: internal")
	DefaultLimit = 10
)

// ErrRefused — отказ от класса с доводом: не находка.
//
//errs:nokind класс зависит от вызывающего
var ErrRefused = errors.New("good: refused")
