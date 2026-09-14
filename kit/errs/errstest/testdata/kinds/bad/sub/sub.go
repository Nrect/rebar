package sub

import "errors"

// Подпакет проверяется тем же вызовом: корень обходится рекурсивно.
var ErrInSubpackage = errors.New("sub: no kind")
