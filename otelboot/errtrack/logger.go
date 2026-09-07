package errtrack

import (
	"context"
	"log/slog"

	"github.com/getsentry/sentry-go"
)

// SkipKey — атрибут-сигнал «эту запись в трекер не дублировать»: ставится на
// Error-лог, событие для которого уже отправлено другим путём (recover шлёт
// панику через CapturePanic — богаче и группируется правильно; без сигнала его
// же Error-лог создал бы второе, мусорное событие).
const SkipKey = "errtrack_skip"

// Skip возвращает атрибут-сигнал для SkipKey.
func Skip() slog.Attr { return slog.Bool(SkipKey, true) }

// WrapLogger оборачивает логгер так, что каждая запись уровня Error и выше
// дублируется событием в трекер. При выключенном трекере обёртка — прозрачный
// passthrough с одной атомарной проверкой.
func WrapLogger(l *slog.Logger) *slog.Logger {
	return slog.New(hookHandler{next: l.Handler()})
}

// hookHandler копит bound-атрибуты (Logger.With / WithAttrs) сам: slog отдаёт
// их только внутреннему обработчику, и без собственной копии событие ушло бы
// голым message, без slug и request_id — весь путь 5xx склеился бы в одну
// бесполезную issue.
type hookHandler struct {
	next  slog.Handler
	attrs []slog.Attr // группы не разворачиваем — плоский срез
}

func (h hookHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h hookHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelError {
		if hub := tracker.Load(); hub != nil {
			capture(hub, h.attrs, rec)
		}
	}
	return h.next.Handle(ctx, rec)
}

func (h hookHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return hookHandler{next: h.next.WithAttrs(attrs), attrs: merged}
}

func (h hookHandler) WithGroup(name string) slog.Handler {
	return hookHandler{next: h.next.WithGroup(name), attrs: h.attrs}
}

// capture строит событие из slog-записи: message = текст записи, атрибуты
// (bound + записи) — в контекст события.
func capture(hub *sentry.Hub, bound []slog.Attr, rec slog.Record) {
	attrs := make(sentry.Context, rec.NumAttrs()+len(bound))
	for _, a := range bound {
		attrs[a.Key] = a.Value.Any()
	}
	skip := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == SkipKey {
			skip = true
			return false
		}
		attrs[a.Key] = a.Value.Any()
		return true
	})
	if skip || attrs[SkipKey] == true {
		return
	}

	hub = hub.Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetLevel(sentry.LevelError)
		if len(attrs) > 0 {
			scope.SetContext("log_attrs", attrs)
		}
		hub.CaptureMessage(rec.Message)
	})
}
