package outboxtest_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// Настройка правится на ходу: тест потребителя меняет её, пока воркер в другой
// горутине зовёт хендлер. Под -race это обязано быть чисто.
func TestRecordingHandler_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	h := outboxtest.NewRecordingHandler()
	whileServing(
		func() { // воркер: Handle по ключу A-1 читает все карты
			for range 300 {
				_ = h.Handle(context.Background(), outbox.Delivery{Kind: "order.paid", AggregateID: "A-1"})
			}
		},
		func(i int) { h.FailFor("A-1", i%2) },
		func(int) { h.PermanentFor("B-2") },
		func(int) { h.ThrottleFor("C-3", time.Minute) },
		func(int) { h.SkipFor("D-4") },
		func(int) { h.PanicFor("E-5", 1) },
		func(i int) {
			if i%2 == 0 {
				h.SetHook(func(context.Context, outbox.Delivery) error { return nil })
				return
			}
			h.SetHook(nil)
		},
		func(int) { _, _ = h.Handled(), h.Panicked("E-5") },
	)
}

// Хук зовётся вне замка: хук, позвавший сам двойник, иначе повесил бы Handle.
func TestRecordingHandler_HookMayCallTheHandler(t *testing.T) {
	t.Parallel()

	h := outboxtest.NewRecordingHandler()
	h.SetHook(func(context.Context, outbox.Delivery) error {
		h.FailFor("A-2", 1)
		_ = h.Handled()
		return nil
	})

	done := make(chan error, 1)
	go func() {
		done <- h.Handle(context.Background(), outbox.Delivery{Kind: "order.paid", AggregateID: "A-1"})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("хук, позвавший двойник, повесил Handle: он зовётся под замком")
	}

	h.SetHook(nil)
	require.ErrorIs(t, h.Handle(context.Background(), outbox.Delivery{Kind: "order.paid", AggregateID: "A-2"}),
		outboxtest.ErrHandlerFailed, "настройка из хука применилась")
}

// Счётчики FailFor и PanicFor правятся и тратятся параллельно. Запись мимо
// замка здесь — не красный тест, а fatal error: concurrent map writes.
func TestRecordingHandler_CountdownsSurviveParallelUse(t *testing.T) {
	t.Parallel()
	const workers, rounds = 8, 300

	h := outboxtest.NewRecordingHandler()
	d := outbox.Delivery{Kind: "order.paid", AggregateID: "A-1"}
	var panics atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				h.FailFor(d.AggregateID, i%3)
				h.PanicFor(d.AggregateID, i%2)
				if handlePanics(h, d) {
					panics.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int(panics.Load()), h.Panicked(d.AggregateID), "каждая паника двойника поймана ровно раз")
	assert.Len(t, h.Handled(), workers*rounds)
}

// handlePanics — Handle с перехватом паники: горутина теста падать не вправе.
func handlePanics(h *outboxtest.RecordingHandler, d outbox.Delivery) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	_ = h.Handle(context.Background(), d)
	return false
}
