package nested

import "errors"

// Вложенный testdata не проверяется: компилятор его не собирает.
var ErrInNestedTestdata = errors.New("nested: skipped")
