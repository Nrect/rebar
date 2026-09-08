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
// поведение задаётся картами. Потокобезопасен.
//
// Ключ карт — AggregateID доставки, а если он пуст — Kind: тест либо ломает
// один заказ, либо весь тип сообщений, и обоим нужен один синтаксис.
type RecordingHandler struct {
	mu       sync.Mutex
	handled  []outbox.Delivery
	panicked map[string]int

	// FailFor — ключ → сколько ближайших вызовов провалить временным сбоем.
	FailFor map[string]int
	// PermanentFor — ключ → постоянный отказ: failed(permanent) без ретраев.
	PermanentFor map[string]bool
	// ThrottleFor — ключ → «приходи не раньше»: повтор в названный срок.
	ThrottleFor map[string]time.Duration
	// SkipFor — ключ → предикат не подтвердился (check-at-send): done(skipped).
	SkipFor map[string]bool
	// PanicFor — ключ → сколько ближайших вызовов уронить паникой.
	PanicFor map[string]int
	// Hook — если задан, отвечает вместо всего остального: так тест проверяет
	// таймаут (подождать ctx.Done) или порядок вызовов.
	Hook func(ctx context.Context, d outbox.Delivery) error
}

// NewRecordingHandler — двойник без отказов.
func NewRecordingHandler() *RecordingHandler {
	return &RecordingHandler{
		panicked:     map[string]int{},
		FailFor:      map[string]int{},
		PermanentFor: map[string]bool{},
		ThrottleFor:  map[string]time.Duration{},
		SkipFor:      map[string]bool{},
		PanicFor:     map[string]int{},
	}
}

// Handle записывает доставку и отвечает по настройкам. Доставка пишется до
// отказа: тест на «ровно один раз» считает попытки, а не успехи.
func (h *RecordingHandler) Handle(ctx context.Context, d outbox.Delivery) error {
	h.mu.Lock()
	hook := h.Hook
	h.handled = append(h.handled, copyDelivery(d))
	h.mu.Unlock()
	// Хук зовётся без замка: он вправе ждать ctx.Done, а двойник — отвечать другим.
	if hook != nil {
		return hook(ctx, d)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.answer(key(d))
}

// answer — ответ по настройкам; порядок повторяет классификацию Drain.
func (h *RecordingHandler) answer(k string) error {
	if left := h.PanicFor[k]; left > 0 {
		h.PanicFor[k] = left - 1
		h.panicked[k]++
		panic("outboxtest: handler panicked for " + k)
	}
	if h.SkipFor[k] {
		return outbox.ErrSkip
	}
	if h.PermanentFor[k] {
		return outbox.Permanent(ErrHandlerFailed)
	}
	if after, throttled := h.ThrottleFor[k]; throttled {
		return outbox.Throttled(ErrHandlerFailed, after)
	}
	if left := h.FailFor[k]; left > 0 {
		h.FailFor[k] = left - 1
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
