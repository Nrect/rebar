package password

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Семафор не пускает больше слотов, чем в нём есть: занятый слот освобождается
// только возвратом, и ожидающий проходит после него, а не рядом с ним.
func TestAcquire_HoldsTheCap(t *testing.T) {
	t.Parallel()

	slots := make(chan struct{}, 2)
	first, err := acquire(t.Context(), slots, time.Second)
	if err != nil {
		t.Fatalf("первый слот: %v", err)
	}
	second, err := acquire(t.Context(), slots, time.Second)
	if err != nil {
		t.Fatalf("второй слот: %v", err)
	}

	// Третий обязан ждать и уйти по MaxWait: слотов больше нет.
	start := time.Now()
	if _, err = acquire(t.Context(), slots, 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("третий слот выдан при полном семафоре: %v", err)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond {
		t.Fatalf("отказ пришёл через %s, MaxWait не отработал", waited)
	}

	// Освобождение пускает следующего.
	second()
	third, err := acquire(t.Context(), slots, time.Second)
	if err != nil {
		t.Fatalf("слот не освободился: %v", err)
	}
	third()
	first()
	if len(slots) != 0 {
		t.Fatalf("после всех возвратов занято %d слотов", len(slots))
	}
}

// Пик занятости под нагрузкой не выше потолка: двадцать четыре горутины на два
// слота. Счётчик ведётся МЕЖДУ acquire и release, поэтому меряется настоящая
// одновременность, а не длина очереди.
func TestAcquire_NeverExceedsCap(t *testing.T) {
	t.Parallel()

	const (
		limit   = 2
		workers = 24
	)
	slots := make(chan struct{}, limit)
	var inFlight, peak atomic.Int64

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			release, err := acquire(context.WithoutCancel(t.Context()), slots, 10*time.Second)
			if err != nil {
				t.Errorf("слот не выдан: %v", err)
				return
			}
			defer release()

			now := inFlight.Add(1)
			for {
				was := peak.Load()
				if now <= was || peak.CompareAndSwap(was, now) {
					break
				}
			}
			time.Sleep(time.Millisecond) // окно, в котором пик виден
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("одновременно работало %d при потолке %d", got, limit)
	} else if got != limit {
		t.Fatalf("занятость не дошла до потолка (%d): нагрузка не создалась", got)
	}
}

// Отмена во время ожидания — это перегрузка, а не таймаут вызывающего: весь
// бюджет запроса ушёл на очередь к семафору. Обёртка ctx.Err сделала бы
// errors.Is(err, context.DeadlineExceeded) истинным, и слой HTTP отдал бы
// клиенту таймаут вместо отказа по занятости.
func TestAcquire_CancelWhileWaitingIsBusy(t *testing.T) {
	t.Parallel()

	slots := make(chan struct{}, 1)
	release, err := acquire(t.Context(), slots, time.Second)
	if err != nil {
		t.Fatalf("слот: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = acquire(ctx, slots, time.Minute)

	if !errors.Is(err, ErrBusy) {
		t.Fatalf("ожидался ErrBusy, получено %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("ErrBusy обернул контекст: %v", err)
	}
}

// Отменённый на входе контекст — отказ вызывающего: очередь не занимали.
func TestAcquire_CancelledBeforeQueueReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := acquire(ctx, make(chan struct{}, 2), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидалась отмена вызывающего, получено %v", err)
	}
	if errors.Is(err, ErrBusy) {
		t.Fatalf("отмена на входе выдана за перегрузку: %v", err)
	}
}

// ПЕРЕГРУЗКА НЕОТЛИЧИМА. Известный логин (Verify) и неизвестный (Equalize)
// отвечают одной ошибкой и одинаковым набором вызовов, иначе сам потолок
// становится каналом перебора адресов: по ответу видно, какой путь выбрал
// сервер. Семафор здесь заполняется напрямую — тест обязан быть
// детерминированным, а не «успеть, пока чужая горутина держит слот».
func TestHasher_BusyIsIdenticalForVerifyAndEqualize(t *testing.T) {
	t.Parallel()

	h := &Hasher{
		params:  cheapParams(),
		slots:   make(chan struct{}, MinSlots),
		maxWait: 5 * time.Millisecond,
	}
	for range MinSlots {
		h.slots <- struct{}{}
	}

	_, verifyErr := h.Verify(t.Context(), "p", validHash(t, h))
	equalizeErr := h.Equalize(t.Context(), "p")
	_, hashErr := h.Hash(t.Context(), "p")

	for name, err := range map[string]error{"Verify": verifyErr, "Equalize": equalizeErr, "Hash": hashErr} {
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("%s при полном семафоре вернул %v", name, err)
		}
	}
	if verifyErr.Error() != equalizeErr.Error() {
		t.Fatalf("тексты отличаются (%q против %q) — по ответу видно, существует ли логин",
			verifyErr, equalizeErr)
	}
}

// Второй хешер с другим числом слотов роняет процесс: молчаливое «победил
// первый» означало бы потолок памяти, о котором никто не просил.
func TestUseGate_RejectsASecondSize(t *testing.T) {
	t.Parallel()

	slots := useGate(MinSlots)
	if cap(slots) != MinSlots {
		t.Fatalf("семафор процесса собран на %d слотов", cap(slots))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("второй размер потолка принят молча")
		}
	}()
	useGate(MaxSlots)
}

// validHash — настоящий хэш на параметрах теста, посчитанный мимо семафора.
func validHash(t *testing.T, h *Hasher) string {
	t.Helper()

	free := &Hasher{params: h.params, slots: make(chan struct{}, 1), maxWait: time.Second}
	encoded, err := free.Hash(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// cheapParams — argon2id на минимальной цене: тесты семафора проверяют
// очередь, а не стойкость хэша.
func cheapParams() params {
	return params{memoryKiB: 8 * 1024, time: 1, threads: 1, keyLen: 32, saltLen: 16}
}
