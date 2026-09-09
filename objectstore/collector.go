package objectstore

import (
	"context"
	"fmt"
	"time"
)

// Collector — уборка объектов, потерявших владельца. Сигнатура Run совпадает
// с scheduler.Job.Run: совпадение сигнатуры — весь контракт, импорта
// планировщика здесь нет (ADR-0005).
type Collector struct {
	store Store
	owned Owned
	cfg   CollectorConfig
	now   func() time.Time
}

// NewCollector паникует на nil-портах и негодном Config.
func NewCollector(store Store, owned Owned, cfg CollectorConfig) *Collector {
	if store == nil {
		panic("objectstore.NewCollector: store must not be nil")
	}
	if owned == nil {
		// Сборщик без порта владения — это rm -rf с таймером (ADR-0006,
		// «Чего нет»).
		panic("objectstore.NewCollector: owned must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("objectstore.NewCollector: " + err.Error())
	}
	return &Collector{
		store: store, owned: owned, cfg: cfg,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
func (c *Collector) SetClock(now func() time.Time) { c.now = now }

// Run обходит префикс и убирает сирот. Возвращает их число: удалённых в режиме
// CollectDelete, найденных в CollectDryRun.
//
// СБОЙ IsOwned ОСТАНАВЛИВАЕТ ПРОГОН. Недоступный источник владения признал бы
// сиротами всех, а «считаем сиротой при ошибке» — это способ удалить бакет
// целиком одной недоступной базой (ADR-0006).
func (c *Collector) Run(ctx context.Context) (int, error) {
	deadline := c.now().Add(-c.cfg.MinAge)
	var collected int
	var cursor string
	for {
		page, err := c.store.List(ctx, c.cfg.Prefix, cursor, c.cfg.BatchSize)
		if err != nil {
			return collected, fmt.Errorf("%w: list", ErrUnavailable)
		}
		n, err := c.sweep(ctx, page.Objects, deadline)
		collected += n
		if err != nil {
			return collected, err
		}
		if page.Cursor == "" {
			return collected, nil
		}
		if page.Cursor == cursor {
			return collected, ErrCursorStuck
		}
		cursor = page.Cursor
	}
}

// sweep — одна страница. Возвращает число убранных и ошибку, на которой
// прогон обязан остановиться.
func (c *Collector) sweep(ctx context.Context, objects []Object, deadline time.Time) (int, error) {
	var collected int
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return collected, err
		}
		// Grace: объект моложе MinAge не трогается никогда. Строгое сравнение
		// в сторону «оставить»: ровно на границе объект ещё молод.
		if !obj.ModifiedAt.Before(deadline) {
			continue
		}
		owned, err := c.owned.IsOwned(ctx, obj.Key)
		if err != nil {
			return collected, fmt.Errorf("%w: ownership is unknown, run stopped", ErrUnavailable)
		}
		if owned {
			continue
		}
		if c.cfg.Mode == CollectDelete {
			if err = c.store.Delete(ctx, obj.Key); err != nil {
				return collected, fmt.Errorf("%w: delete", ErrUnavailable)
			}
		}
		collected++
	}
	return collected, nil
}
