package password

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// ПОТОЛОК ОДИН НА ПРОЦЕСС, И ЭТО НЕ СОГЛАШЕНИЕ, А КОНСТРУКЦИЯ. Два реалма
// (покупатели и персонал) — это два экземпляра сервиса сессий, и если бы
// семафор жил в хешере, их лимиты сложились бы: восемь одновременных хэшей
// по 64 MiB вместо четырёх. Поэтому семафор пакетный, размер задаётся первым
// NewHasher, а второй с другим размером роняет процесс на старте — молчаливое
// «победил первый» означало бы потолок, о котором никто не просил.
var gate struct {
	once  sync.Once
	slots chan struct{}
	size  int
}

// useGate возвращает семафор процесса, заводя его при первом обращении.
func useGate(size int) chan struct{} {
	gate.once.Do(func() {
		gate.slots = make(chan struct{}, size)
		gate.size = size
	})
	if gate.size != size {
		panic("password.NewHasher: process-wide cap is already " + strconv.Itoa(gate.size) +
			" slots, a hasher with " + strconv.Itoa(size) + " would raise it")
	}
	return gate.slots
}

// acquire занимает слот. Свободный слот берётся без таймера — на этот путь
// приходятся все запросы, кроме всплеска.
func acquire(ctx context.Context, slots chan struct{}, maxWait time.Duration) (func(), error) {
	// Отменённый контекст на входе — это отказ вызывающего, а не наша
	// перегрузка: очередь мы ещё не занимали, и врать про занятость нельзя.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
	}

	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-timer.C:
		return nil, ErrBusy
	case <-ctx.Done():
		// ErrBusy БЕЗ ОБЁРТКИ ctx.Err(): весь бюджет запроса ушёл на ожидание
		// слота, то есть это перегрузка. Обёртка сделала бы errors.Is(err,
		// context.DeadlineExceeded) истинным, и слой HTTP отдал бы клиенту
		// таймаут вместо отказа по занятости — разные инцидент и алерт.
		return nil, ErrBusy
	}
}
