package logotel_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/kit/reqid"

	"github.com/nrect/rebar/examples/monolith/logotel"
)

// TestNew_AddsContextKeys — идентификаторы запроса, трассы и субъекта приходят
// в запись из контекста, в том числе через Logger.With: вызывающий код их не
// пишет и потому не забывает.
func TestNew_AddsContextKeys(t *testing.T) {
	subject := uuid.New()
	traceID := trace.TraceID{0x0a, 0xf7, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	ctx := reqid.With(t.Context(), "req-1")
	ctx = authhttp.WithPrincipal(ctx, auth.Principal{Realm: "shop", SubjectID: subject})
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	}))

	var buf bytes.Buffer
	logotel.New(&buf, slog.LevelInfo).With(slog.String("job", "x")).ErrorContext(ctx, "cron job failed")

	rec := decode(t, &buf)
	require.Equal(t, "req-1", rec["request_id"])
	require.Equal(t, traceID.String(), rec["trace_id"])
	require.Equal(t, subject.String(), rec["subject_id"])
	require.Equal(t, "x", rec["job"])
}

// TestNew_NoContextNoKeys — нет спана и сессии — нет ключа: пустые значения
// засоряли бы поиск.
func TestNew_NoContextNoKeys(t *testing.T) {
	var buf bytes.Buffer
	logotel.New(&buf, slog.LevelInfo).InfoContext(t.Context(), "started")

	rec := decode(t, &buf)
	for _, key := range []string{"request_id", "trace_id", "subject_id"} {
		require.NotContains(t, rec, key)
	}
}

// TestNew_RequestIDOnce — httperr кладёт request_id сам, и второй такой ключ в
// JSON был бы дублем.
func TestNew_RequestIDOnce(t *testing.T) {
	var buf bytes.Buffer
	ctx := reqid.With(t.Context(), "req-1")
	logotel.New(&buf, slog.LevelInfo).ErrorContext(ctx, "http error", slog.String("request_id", "req-1"))

	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte(`"request_id"`)), buf.String())
}

// TestNew_LevelFollowsVar — уровень меняется после чтения конфига: логгер
// ставится раньше него.
func TestNew_LevelFollowsVar(t *testing.T) {
	var (
		buf   bytes.Buffer
		level slog.LevelVar
	)
	log := logotel.New(&buf, &level)
	log.DebugContext(t.Context(), "cron job done")
	require.Zero(t, buf.Len(), "по умолчанию Info: Debug не пишется")

	level.Set(slog.LevelDebug)
	log.DebugContext(t.Context(), "cron job done")
	require.NotZero(t, buf.Len())
}

// TestNew_TimeInUTC — время записи в UTC, какой бы пояс ни был у момента: записи
// двух сервисов сходятся по времени. Момент — в чужом поясе явно: t.Setenv("TZ")
// не меняет уже загруженный time.Local.
func TestNew_TimeInUTC(t *testing.T) {
	at := time.Date(2031, time.March, 9, 2, 30, 15, 0, time.FixedZone("UTC+05:45", 5*60*60+45*60))
	var buf bytes.Buffer
	log := logotel.New(&buf, slog.LevelInfo)
	require.NoError(t, log.Handler().Handle(t.Context(), slog.NewRecord(at, slog.LevelInfo, "started", 0)))

	require.Equal(t, "2031-03-08T20:45:15Z", decode(t, &buf)["time"])
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec), buf.String())
	return rec
}
