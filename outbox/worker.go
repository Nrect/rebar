package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Worker — фоновая половина: забирает пачку под аренду, отдаёт хендлерам,
// записывает исходы. Плюс операторские методы dead-letter.
type Worker struct {
	store Store
	reg   *Registry
	// kinds — снимок реестра на момент сборки: он же уходит в Claim фильтром
	// и в Stats списком известных. Реестр после NewWorker не меняется.
	kinds    []Kind
	cfg      Config
	now      func() time.Time
	newToken func() uuid.UUID
}

// NewWorker паникует на nil-портах и негодном Config: ошибка проводки обязана
// падать на старте. Ошибку возвращает там, где виноват не код, а расхождение
// двух списков потребителя.
//
// Реестр обязан быть подмножеством Config.Kinds: тип, известный воркеру, но
// не объявленный в конфиге, никогда не будет вставлен (Prepare отвергнет
// его), то есть хендлер написан впустую — и об этом лучше узнать на старте.
func NewWorker(store Store, reg *Registry, cfg Config) (*Worker, error) {
	if store == nil {
		panic("outbox.NewWorker: store must not be nil")
	}
	if reg == nil {
		panic("outbox.NewWorker: registry must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("outbox.NewWorker: " + err.Error())
	}
	kinds := reg.Kinds()
	if len(kinds) == 0 {
		return nil, fmt.Errorf("%w: registry has no handlers", ErrBadKind)
	}
	for _, k := range kinds {
		if !cfg.knowsKind(k) {
			return nil, fmt.Errorf("%w: handler kind %q is not listed in Config.Kinds", ErrBadKind, k)
		}
	}
	return &Worker{
		store: store, reg: reg, kinds: kinds, cfg: cfg,
		now:      func() time.Time { return time.Now().UTC() },
		newToken: uuid.New,
	}, nil
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
func (w *Worker) SetClock(now func() time.Time) { w.now = now }

// Kinds — типы, которые умеет этот воркер (снимок реестра).
func (w *Worker) Kinds() []Kind { return append([]Kind(nil), w.kinds...) }

// Purge — задача планировщика (сигнатура scheduler.Job.Run): удаляет done и
// expired старше Retention, до BatchSize за вызов. failed не трогает — это
// dead-letter, его разбирает человек (doc.go, п. 7).
func (w *Worker) Purge(ctx context.Context) (int, error) {
	deleted, err := w.store.Purge(ctx, w.now().Add(-w.cfg.Retention), w.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("%w: purge: %w", ErrUnavailable, err)
	}
	return deleted, nil
}

// Stats — снимок очереди для гейджей; потребитель зовёт его по своему
// расписанию, а не коллбэком на каждый scrape (CONVENTIONS §6).
func (w *Worker) Stats(ctx context.Context) (Stats, error) {
	stats, err := w.store.Stats(ctx, w.now(), w.kinds)
	if err != nil {
		return Stats{}, fmt.Errorf("%w: stats: %w", ErrUnavailable, err)
	}
	return stats, nil
}

// ListFailed — dead-letter для оператора, самые старые первыми.
func (w *Worker) ListFailed(ctx context.Context, limit int) ([]Envelope, error) {
	rows, err := w.store.ListFailed(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list failed: %w", ErrUnavailable, err)
	}
	return rows, nil
}

// Redrive возвращает строку из dead-letter в работу: только из failed, со
// сбросом попыток. false — строки нет либо она не в failed, и это не ошибка:
// повторный клик оператора не должен выглядеть сбоем.
//
// Аудит — на потребителе: кто нажал и зачем, знает он, а не пакет
// (doc.go, п. 8).
func (w *Worker) Redrive(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := w.store.Redrive(ctx, id, w.now())
	if err != nil {
		return false, fmt.Errorf("%w: redrive: %w", ErrUnavailable, err)
	}
	return ok, nil
}
