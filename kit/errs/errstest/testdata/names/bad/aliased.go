package bad

import (
	stderrors "errors"

	kiterrs "github.com/nrect/rebar/kit/errs"
)

// Импорты под псевдонимами узнаются по пути, а не по имени.
var (
	ErrAliasedPlain   = stderrors.New("aliased without a package")
	ErrAliasedKinded  = kiterrs.Kinded(kiterrs.KindTimeout, "aliased: wrong package name")
	ErrAliasedPrefix  = kiterrs.Kinded(kiterrs.KindTimeout, "bad: aliased and prefixed")
	ErrAliasedWrapped = kiterrs.Kinded(kiterrs.KindConflict, "bad: aliased and classed")
)
