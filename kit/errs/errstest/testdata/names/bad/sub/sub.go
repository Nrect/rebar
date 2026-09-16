package sub

import "errors"

// Подпакет проверяется тем же вызовом, и префикс у него СВОЙ: имя берётся из
// package этого файла, а не корня обхода.
var (
	ErrInSubpackage = errors.New("bad: prefix of the root package, not of this one")
	ErrOwnPrefix    = errors.New("sub: own package names itself")
)
