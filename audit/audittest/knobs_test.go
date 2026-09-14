package audittest_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/audittest"
)

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: Write читает
// настройку под замком, и правиться она обязана под ним же.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{reflect.TypeFor[audittest.Sink]()} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Отказ журнала правится на ходу: тест потребителя включает его, пока ручка
// сервера в другой горутине пишет события. Под -race это обязано быть чисто.
func TestSink_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	whileServing(
		func() { // ручка сервера: Write читает err
			for range 300 {
				_ = sink.Write(t.Context(), event("a", audit.OutcomeSuccess))
			}
		},
		func(i int) { sink.SetErr(flip(i)) },
		func(int) { _, _ = sink.Events(), sink.Count() },
	)
}

// flip — отказ на чётном круге и nil на нечётном: ручка то ставится, то снимается.
func flip(i int) error {
	if i%2 == 0 {
		return audittest.ErrSinkFailed
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
