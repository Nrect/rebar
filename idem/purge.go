package idem

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Размер уборки: пачка за вызов Pruner.Purge и потолок пачек за прогон.
// Недоделанное доделывает следующий прогон, а прогон не держит остановку
// процесса дольше сотни коротких DELETE.
const (
	PurgeBatchSize     = 1000
	PurgeBatchesPerRun = 100
)

// Purger — уборка записей старше Config.Retention; Run ложится в
// scheduler.Job.Run без обёртки.
type Purger struct {
	pruner    Pruner
	retention time.Duration
	now       func() time.Time
}

// NewPurger паникует на nil-хранилище и негодном Config: ошибка конфигурации
// падает на старте, а не в первом прогоне.
func NewPurger(pruner Pruner, cfg Config) *Purger {
	if pruner == nil {
		panic("idem.NewPurger: pruner must not be nil")
	}
	if err := cfg.Validate(); err != nil {
		panic("idem.NewPurger: " + err.Error())
	}
	return &Purger{pruner: pruner, retention: cfg.Retention, now: time.Now}
}

// SetClock подменяет источник времени; только для тестов и до начала работы.
// nil — паника здесь, а не разыменование в чужом стеке.
func (p *Purger) SetClock(now func() time.Time) {
	if now == nil {
		panic("idem.Purger.SetClock: now must not be nil")
	}
	p.now = now
}

// Run удаляет записи старше срока пачками и возвращает число удалённых.
// Незакоммиченных записей уборка не видит. Отмена между пачками — причина
// отмены, а не ErrUnavailable: остановка не выглядит сбоем хранилища.
func (p *Purger) Run(ctx context.Context) (int, error) {
	before := p.now().Add(-p.retention)
	deleted := 0
	for range PurgeBatchesPerRun {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		n, err := p.pruner.Purge(ctx, before, PurgeBatchSize)
		if err != nil {
			return deleted, storeError("purge", err)
		}
		deleted += n
		if n < PurgeBatchSize {
			break
		}
	}
	return deleted, nil
}

// storeError — сбой хранилища в ErrUnavailable; уже завёрнутый не
// заворачивается дважды.
func storeError(op string, err error) error {
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}
