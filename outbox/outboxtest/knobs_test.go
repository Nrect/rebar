package outboxtest_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// errDown — сбой хранилища, который тест включает через SetErr и SetFinishErr.
var errDown = errors.New("outboxtest_test: store is down")

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: методы порта
// читают настройку под замком, и правиться она обязана под ним же.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[outboxtest.RecordingHandler](),
		reflect.TypeFor[outboxtest.MemStore](),
		reflect.TypeFor[outboxtest.Clock](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Сбои и хук хранилища правятся на ходу: тест потребителя включает их, пока
// воркер в другой горутине гоняет очередь. Под -race это обязано быть чисто.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := outboxtest.NewMemStore()
	ctx := context.Background()
	whileServing(
		func() { // воркер: Finish читает хук, err и finishErr, Stats — err
			for range 300 {
				_ = store.Finish(ctx, outbox.FinishRequest{ID: uuid.New(), Token: uuid.New(), Outcome: outbox.FinishDone, Now: at})
				_, _ = store.Stats(ctx, at, nil)
			}
		},
		func(i int) { store.SetErr(flip(i)) },
		func(i int) { store.SetFinishErr(flip(i)) },
		func(i int) {
			if i%2 == 0 {
				store.SetAfterHandle(func() {})
				return
			}
			store.SetAfterHandle(nil)
		},
		func(int) { _ = store.Rows() },
	)
}

// Хук AfterHandle зовётся вне замка: хук, позвавший само хранилище, иначе
// повесил бы Finish. Так устроен и «перезапуск» в тестах ядра: хук снимает себя.
func TestMemStore_AfterHandleMayCallTheStore(t *testing.T) {
	t.Parallel()

	store := outboxtest.NewMemStore()
	calls := 0
	store.SetAfterHandle(func() {
		calls++
		_ = store.Rows()
		store.SetAfterHandle(nil)
	})
	finish := func() error {
		return store.Finish(context.Background(), outbox.FinishRequest{ID: uuid.New(), Token: uuid.New()})
	}

	done := make(chan error, 1)
	go func() { done <- finish() }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, outbox.ErrClaimLost, "хук отработал, Finish дошёл до fencing")
	case <-time.After(5 * time.Second):
		t.Fatal("хук, позвавший хранилище, повесил Finish: он зовётся под замком")
	}

	require.ErrorIs(t, finish(), outbox.ErrClaimLost)
	assert.Equal(t, 1, calls, "хук снял себя изнутри: второй Finish его не звал")
}

// flip — сбой на чётном круге и nil на нечётном: ручка то ставится, то снимается.
func flip(i int) error {
	if i%2 == 0 {
		return errDown
	}
	return nil
}

// whileServing крутит каждую ручку в своей горутине, пока serve не отработает.
// Своя горутина — не прихоть: ручка, пишущая мимо замка, не делит с serve ни
// одной точки синхронизации, и -race видит гонку при любом порядке. В общей
// горутине её прятали бы замки соседних ручек.
func whileServing(serve func(), knobs ...func(i int)) {
	served := make(chan struct{})
	go func() {
		defer close(served)
		serve()
	}()
	var wg sync.WaitGroup
	for _, knob := range knobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				knob(i) // до проверки: каждая ручка тронута хотя бы раз
				select {
				case <-served:
					return
				default:
				}
			}
		}()
	}
	wg.Wait()
}
