package objectstoretest_test

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// errDown — отказ хранилища, который тест включает через SetErr.
var errDown = errors.New("objectstoretest_test: store is down")

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: методы порта
// читают настройку под замком, и правиться она обязана под ним же.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[objectstoretest.MemStore](),
		reflect.TypeFor[objectstoretest.MemOwned](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Отказ и часы хранилища правятся на ходу: тест потребителя меняет их, пока
// ручка загрузки в другой горутине кладёт файлы. Под -race это обязано быть
// чисто.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := objectstoretest.NewMemStore()
	body := objectstoretest.PNG(32)
	moment := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	whileServing(
		func() { // ручка загрузки: Put читает err и часы, List — err
			for range 300 {
				_, _ = store.Put(t.Context(), objectstore.PutRequest{
					Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
				})
				_, _ = store.List(t.Context(), "uploads", "", 10)
			}
		},
		func(i int) { store.SetErr(flip(i)) },
		func(i int) {
			if i%2 == 0 {
				store.SetClock(func() time.Time { return moment })
				return
			}
			store.SetClock(func() time.Time { return moment.Add(time.Hour) })
		},
		func(int) { _ = store.Keys() },
	)
}

// Отказ источника владения правится на ходу, пока сборщик в другой горутине
// спрашивает о ключах.
func TestMemOwned_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	owned := objectstoretest.NewMemOwned("uploads/a.png")
	whileServing(
		func() { // сборщик: IsOwned читает err
			for range 300 {
				_, _ = owned.IsOwned(t.Context(), "uploads/a.png")
			}
		},
		func(i int) { owned.SetErr(flip(i)) },
		func(int) { _, _ = owned.Keys(), owned.Calls() },
	)
}

// flip — отказ на чётном круге и nil на нечётном: ручка то ставится, то снимается.
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
