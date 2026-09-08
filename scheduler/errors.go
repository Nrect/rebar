package scheduler

import "errors"

// ErrPanic — паника задачи, превращённая в ошибку прогона: recover не даёт ей
// уронить процесс (doc.go, п. 1). Run.Panicked — то же самое поле в лоб.
var ErrPanic = errors.New("scheduler: job panicked")

// ErrUnknownJob — RunNow с именем, которого нет в наборе: опечатка в ручном
// запуске обязана быть видимой, а не тихим no-op.
var ErrUnknownJob = errors.New("scheduler: unknown job")
