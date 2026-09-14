package outboxtest

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/nrect/rebar/outbox"
)

// ErrHandlerFailed — временный сбой двойника. Не постоянный и не throttled:
// Drain обязан назначить повтор по backoff, а не увести строку в dead-letter.
var ErrHandlerFailed = errors.New("outboxtest: handler is temporarily unavailable")

// RecordingHandler — записывающий outbox.Handler: доставки складываются,
// поведение задаётся методами. Потокобезопасен целиком, включая настройку.
//
// Ключ настройки — AggregateID доставки, а если он пуст — Kind: тест либо
// ломает один заказ, либо весь тип сообщений, и обоим нужен один синтаксис.
//
// Публичных полей нет: Handle читает настройку под замком, а тест потребителя
// правит её, пока воркер в другой горутине зовёт хендлер (CONVENTIONS §3).
type RecordingHandler struct {
	mu       sync.Mutex
	handled  []outbox.Delivery
	panicked map[string]int

	failFor      map[string]int
	permanentFor map[string]bool
	throttleFor  map[string]time.Duration
	skipFor      map[string]bool
	panicFor     map[string]int
	hook         func(ctx context.Context, d outbox.Delivery) error
}

// NewRecordingHandler — двойник без отказов.
func NewRecordingHandler() *RecordingHandler {
	return &RecordingHandler{
		panicked:     map[string]int{},
		failFor:      map[string]int{},
		permanentFor: map[string]bool{},
		throttleFor:  map[string]time.Duration{},
		skipFor:      map[string]bool{},
		panicFor:     map[string]int{},
	}
}

// FailFor — ближайшие times вызовов по ключу k провалить временным сбоем.
// Заменяет прежний счётчик; ноль снимает.
func (h *RecordingHandler) FailFor(k string, times int) { h.set(func() { h.failFor[k] = times }) }

// PermanentFor — постоянный отказ по ключу k: failed(permanent) без ретраев.
func (h *RecordingHandler) PermanentFor(k string) { h.set(func() { h.permanentFor[k] = true }) }

// ThrottleFor — «приходи не раньше after» по ключу k: повтор в названный срок.
func (h *RecordingHandler) ThrottleFor(k string, after time.Duration) {
	h.set(func() { h.throttleFor[k] = after })
}

// SkipFor — предикат по ключу k не подтвердился (check-at-send): done(skipped).
func (h *RecordingHandler) SkipFor(k string) { h.set(func() { h.skipFor[k] = true }) }

// PanicFor — ближайшие times вызовов по ключу k уронить паникой; ноль снимает.
func (h *RecordingHandler) PanicFor(k string, times int) { h.set(func() { h.panicFor[k] = times }) }

// SetHook — если задан, отвечает вместо настройки: так тест проверяет таймаут
// (подождать ctx.Done) или порядок вызовов; nil снимает. Зовётся вне замка и
// вправе звать сам двойник.
func (h *RecordingHandler) SetHook(hook func(ctx context.Context, d outbox.Delivery) error) {
	h.set(func() { h.hook = hook })
}

// set — правка настройки под тем же замком, под которым её читает Handle.
func (h *RecordingHandler) set(mutate func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	mutate()
}

// Handle записывает доставку и отвечает по настройке. Доставка пишется до
// отказа: тест на «ровно один раз» считает попытки, а не успехи.
func (h *RecordingHandler) Handle(ctx context.Context, d outbox.Delivery) error {
	h.mu.Lock()
	hook := h.hook
	h.handled = append(h.handled, copyDelivery(d))
	h.mu.Unlock()
	// Хук зовётся без замка: он вправе ждать ctx.Done и звать сам двойник.
	if hook != nil {
		return hook(ctx, d)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.answer(key(d))
}

// answer — ответ по настройке; порядок повторяет классификацию Drain.
func (h *RecordingHandler) answer(k string) error {
	if left := h.panicFor[k]; left > 0 {
		h.panicFor[k] = left - 1
		h.panicked[k]++
		panic("outboxtest: handler panicked for " + k)
	}
	if h.skipFor[k] {
		return outbox.ErrSkip
	}
	if h.permanentFor[k] {
		return outbox.Permanent(ErrHandlerFailed)
	}
	if after, throttled := h.throttleFor[k]; throttled {
		return outbox.Throttled(ErrHandlerFailed, after)
	}
	if left := h.failFor[k]; left > 0 {
		h.failFor[k] = left - 1
		return ErrHandlerFailed
	}
	return nil
}

// Handled — копии доставок в порядке поступления.
func (h *RecordingHandler) Handled() []outbox.Delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]outbox.Delivery, len(h.handled))
	for i, d := range h.handled {
		out[i] = copyDelivery(d)
	}
	return out
}

// Panicked — сколько раз двойник уронил себя паникой по этому ключу.
func (h *RecordingHandler) Panicked(k string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.panicked[k]
}

// key — AggregateID, а при пустом — Kind.
func key(d outbox.Delivery) string {
	if d.AggregateID != "" {
		return d.AggregateID
	}
	return string(d.Kind)
}

func copyDelivery(d outbox.Delivery) outbox.Delivery {
	out := d
	out.Payload = bytes.Clone(d.Payload)
	out.Headers = maps.Clone(d.Headers)
	out.NotAfter = copyTime(d.NotAfter)
	return out
}
