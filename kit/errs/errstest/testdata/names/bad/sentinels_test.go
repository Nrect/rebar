package bad

import "errors"

// _test.go не проверяется: sentinel'ы прода объявлены не здесь.
var ErrInTestFile = errors.New("in a test file without a package")
