package sesfake

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: ручка сервера
// читает настройку под замком, и правиться она обязана под ним же.
func TestHandler_HasNoExportedFields(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[Handler]()
	for i := range typ.NumField() {
		assert.False(t, typ.Field(i).IsExported(), "поле Handler.%s публичное", typ.Field(i).Name)
	}
}

// Настройка правится на ходу: тест меняет её, пока ручка живого сервера в своей
// горутине принимает письма. Под -race это обязано быть чисто.
func TestHandler_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	h, url := newServer(t)
	body := simpleBody("teacher@school.ru", nil)
	whileServing(
		func() { // клиент; запрос обслуживает горутина сервера, она и читает настройку
			for range 50 {
				if _, err := postStatus(t.Context(), url, body); err != nil {
					t.Error(err)
					return
				}
			}
		},
		func(int) { h.RejectFor("other@school.ru", "MessageRejected") },
		func(i int) { h.ThrottleFor("teacher@school.ru", i%2) },
		func(int) { h.SetSecret("") },
		func(int) { h.SetRegion("ru-central1") },
		func(i int) { h.SetStoreLimit(i % 5) },
		func(int) { h.SetName("probe") },
		func(int) { h.SetOnAccepted(func(SentEmail) {}) },
		func(int) { _ = h.Sent() },
	)
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

// OnAccepted зовётся вне замка: хук, позвавший сам обработчик, иначе повесил бы
// ручку. Ручка зовётся напрямую: повисший запрос живого сервера повесил бы и Close.
func TestHandler_OnAcceptedMayCallTheHandler(t *testing.T) {
	t.Parallel()

	h := NewHandler()
	h.SetOnAccepted(func(SentEmail) {
		h.ThrottleFor("other@school.ru", 1)
		_ = h.Sent()
	})
	req, err := newRequest(t.Context(), "http://sesfake.test", simpleBody("teacher@school.ru", nil), "")
	require.NoError(t, err)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(5 * time.Second):
		t.Fatal("хук, позвавший обработчик, повесил ручку: он зовётся под замком")
	}

	req, err = newRequest(t.Context(), "http://sesfake.test", simpleBody("other@school.ru", nil), "")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "настройка из хука применилась")
}

// Счётчик ThrottleFor правится и тратится параллельно. Запись мимо замка здесь —
// не красный тест, а fatal error: concurrent map writes. accept зовётся напрямую:
// декремент живёт в нём, а разбор HTTP лишь разбавил бы гонку.
func TestHandler_ThrottleForSurvivesParallelUse(t *testing.T) {
	t.Parallel()
	const workers, rounds = 8, 300

	h := NewHandler()
	var throttled atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				h.ThrottleFor("teacher@school.ru", i%3)
				if _, _, fail := h.accept(SentEmail{}, "teacher@school.ru"); fail != nil {
					throttled.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, workers*rounds, int(throttled.Load())+len(h.Sent()),
		"каждое письмо либо принято, либо получило 429, и ровно одно из двух")
}
