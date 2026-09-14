package mailtest_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: методы порта
// читают настройку под замком, и правиться она обязана под ним же. SESServer в
// списке нет: его публичное поле — встроенный обработчик, и страж у того свой,
// в internal/sesfake.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[mailtest.Transport](),
		reflect.TypeFor[mailtest.MemStore](),
		reflect.TypeFor[mailtest.MemSuppressor](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Сбой хранилища правится на ходу: тест потребителя включает его, пока воркер в
// другой горутине гоняет очередь. Под -race это обязано быть чисто.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := mailtest.NewMemStore()
	ctx := context.Background()
	whileServing(
		func() { // воркер: Stats читает err, Finish — err и finishErr
			for range 300 {
				_, _ = store.Stats(ctx, storeBase)
				_ = store.Finish(ctx, mail.FinishRequest{ID: uuid.New(), Outcome: mail.FinishSent, Now: storeBase})
			}
		},
		func(i int) { store.SetErr(flip(i)) },
		func(i int) { store.SetFinishErr(flip(i)) },
		func(int) { _ = store.Rows() },
	)
}

// Сбой стоп-листа правится на ходу, пока ручка сервера проверяет адреса. Под
// -race это обязано быть чисто.
func TestMemSuppressor_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	supp := mailtest.NewMemSuppressor()
	ctx := context.Background()
	whileServing(
		func() { // ручка сервера: оба метода порта читают err
			for range 300 {
				_, _, _ = supp.IsSuppressed(ctx, "teacher@school.ru")
				_ = supp.Suppress(ctx, mail.Suppression{Email: "teacher@school.ru", Reason: mail.SuppressHardBounce})
			}
		},
		func(i int) { supp.SetErr(flip(i)) },
		func(int) { _ = supp.Suppressions() },
	)
}

// flip — сбой на чётном круге и nil на нечётном: ручка то ставится, то снимается.
func flip(i int) error {
	if i%2 == 0 {
		return errUnavailable
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
