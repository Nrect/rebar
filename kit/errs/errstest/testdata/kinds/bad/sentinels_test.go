package bad

import "errors"

// _test.go не проверяется: в проде этой sentinel нет.
var ErrOnlyInTests = errors.New("bad: test only")
