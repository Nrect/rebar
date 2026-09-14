package bad

import (
	stderrors "errors"

	kiterrs "github.com/nrect/rebar/kit/errs"
)

// Импорты под псевдонимами узнаются по пути, а не по имени.
var (
	ErrAliasedPlain   = stderrors.New("bad: aliased plain")
	ErrAliasedUnknown = kiterrs.Kinded(kiterrs.KindUnknown, "bad: aliased unknown")
	ErrAliasedClassed = kiterrs.Kinded(kiterrs.KindTimeout, "bad: aliased classed")
)
