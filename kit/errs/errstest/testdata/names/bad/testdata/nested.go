package nested

import "errors"

// Вложенный testdata пропускается: это корпус чужого стража, а не код прода.
var ErrNested = errors.New("nested without a package")
