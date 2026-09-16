// Package logotel — логгер процесса (docs/CONSUMER.md, §§1 и 7): JSON в поток,
// записи Error — ещё и в трекер ошибок, а request_id, trace_id и subject_id
// дописывает обработчик из контекста, а не вызывающий код.
//
// Каталог назван *otel не для вида: trace_id читается из спана otel, а otel
// законен только в таких каталогах (depguard в корне репозитория).
package logotel

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/kit/reqid"
	"github.com/nrect/rebar/otelboot/errtrack"
)

// Ключи из словаря контракта: по ним ищут записи и блоков, и проекта.
const (
	keyRequestID = "request_id"
	keyTraceID   = "trace_id"
	keySubjectID = "subject_id"
)

// New — логгер процесса. level меняют после чтения конфига: логгер ставится
// раньше, чтобы и ошибка конфига ушла JSON-записью.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	// Обработчик контекста снаружи трекера: событие получает те же ключи, что
	// и строка лога.
	tracked := errtrack.WrapLogger(slog.New(base)).Handler()
	return slog.New(ctxHandler{next: tracked})
}

// ctxHandler дописывает идентификаторы из контекста: запись без request_id не
// находится по нему никогда, а руками его забывают.
type ctxHandler struct{ next slog.Handler }

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	// httperr кладёт request_id сам: второй такой ключ в JSON — дубль.
	if id := reqid.From(ctx); id != "" && !hasKey(r, keyRequestID) {
		r.AddAttrs(slog.String(keyRequestID, id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		r.AddAttrs(slog.String(keyTraceID, sc.TraceID().String()))
	}
	// Идентификатор, а не логин: логин — персональные данные.
	if p, ok := authhttp.PrincipalFrom(ctx); ok {
		r.AddAttrs(slog.String(keySubjectID, p.SubjectID.String()))
	}
	return h.next.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return ctxHandler{next: h.next.WithAttrs(as)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{next: h.next.WithGroup(name)}
}

func hasKey(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}
