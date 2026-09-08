package httperr_test

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// capturingHandler — двойник slog.Handler: копит записи, чтобы тест смотрел
// на уровень и атрибуты, а не на текст строки.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func (h *capturingHandler) only() (slog.Record, bool) {
	records := h.all()
	if len(records) != 1 {
		return slog.Record{}, false
	}
	return records[0], true
}

func attrOf(record slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	ok := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			found, ok = attr.Value, true
			return false
		}
		return true
	})
	return found, ok
}

func newLogger() (*slog.Logger, *capturingHandler) {
	handler := &capturingHandler{}
	return slog.New(handler), handler
}

// retryable — ошибка со структурным контрактом Retry-After.
type retryable struct {
	error
	delay time.Duration
	ok    bool
}

func (e retryable) RetryAfter() (time.Duration, bool) { return e.delay, e.ok }

func (e retryable) Unwrap() error { return e.error }

// startedWriter — обёртка, сообщающая, что ответ уже начат (форма gin).
type startedWriter struct {
	http.ResponseWriter
	written bool
}

func (w startedWriter) Written() bool { return w.written }

// statusWriter — обёртка формы chi: ноль означает «ещё не писали».
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w statusWriter) Status() int { return w.status }
