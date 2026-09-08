package errtrack

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/stretchr/testify/require"
)

// mockTransport ловит события вместо сети. Потокобезопасен: пакет клонирует
// хаб на каждый захват, и тесты идут под -race.
type mockTransport struct {
	mu       sync.Mutex
	events   []*sentry.Event
	flushOK  bool
	configed bool
}

func (m *mockTransport) Flush(time.Duration) bool              { return m.flushOK }
func (m *mockTransport) FlushWithContext(context.Context) bool { return m.flushOK }
func (m *mockTransport) Configure(sentry.ClientOptions)        { m.configed = true }
func (m *mockTransport) Close()                                {}

func (m *mockTransport) SendEvent(e *sentry.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

func (m *mockTransport) sent() []*sentry.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*sentry.Event(nil), m.events...)
}

// enable включает трекер на мок-транспорте и гасит его после теста.
func enable(t *testing.T) *mockTransport {
	t.Helper()
	mt := &mockTransport{flushOK: true}
	_, err := initWith(sentry.ClientOptions{
		Dsn: "https://key@localhost/1", Release: "test", Transport: mt,
	})
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Store(nil) })
	return mt
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// Без DSN трекер выключен: ошибки нет, flush и захваты — дешёвые пустышки.
// Это состояние по умолчанию на dev и в тестах потребителя.
func TestInit_NoDSNIsSilentNoop(t *testing.T) {
	flush, err := Init("", "development", "dev")

	require.NoError(t, err)
	require.Nil(t, tracker.Load())
	require.NotPanics(t, func() {
		require.NoError(t, flush(t.Context()))
		CapturePanic("boom", []byte("stack"))
		CaptureException(errors.New("nope"))
		WrapLogger(discardLogger()).Error("ошибка без трекера")
	})
}

// Кривой DSN — ошибка без самого DSN: sentry печатает разобранный URL целиком,
// а в нём ключ проекта, и эта ошибка уходит в лог старта.
func TestInit_BadDSNDoesNotLeakIt(t *testing.T) {
	const dsn = "https://supersecretkey@sentry.example/%zz"

	flush, err := Init(dsn, "production", "1.0.0")

	require.Error(t, err)
	require.Nil(t, flush)
	require.NotContains(t, err.Error(), "supersecretkey")
	require.NotContains(t, err.Error(), "sentry.example")
	require.Nil(t, tracker.Load(), "неудачный Init трекер не включает")
}

// flush сообщает о неуспевшем буфере ошибкой: иначе потребитель считал бы
// события отправленными и гасил процесс.
func TestFlush_ReportsUnsentBuffer(t *testing.T) {
	mt := &mockTransport{flushOK: false}
	flush, err := initWith(sentry.ClientOptions{Dsn: "https://key@localhost/1", Release: "test", Transport: mt})
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Store(nil) })

	require.Error(t, flush(t.Context()))
}

func TestCaptureException_Sends(t *testing.T) {
	mt := enable(t)

	CaptureException(errors.New("платёж не прошёл"))
	CaptureException(nil)

	events := mt.sent()
	require.Len(t, events, 1, "nil-ошибка события не создаёт")
	require.Equal(t, "платёж не прошёл", events[0].Exception[0].Value)
}

// Паника уходит событием уровня fatal, стек — тот, что снят в defer.
func TestCapturePanic_SendsWithStack(t *testing.T) {
	mt := enable(t)

	CapturePanic("boom", []byte("goroutine 1 [running]:\nmain.handler()"))
	CapturePanic(nil, nil)

	events := mt.sent()
	require.Len(t, events, 1, "nil-значение паники события не создаёт")
	require.Equal(t, sentry.LevelFatal, events[0].Level)
	require.Equal(t, "boom", events[0].Message)
	require.Contains(t, events[0].Contexts["panic"]["stack"], "main.handler()")
}

// Error-запись уносит и свои атрибуты, и bound (Logger.With) — иначе события
// приходили бы голым message и склеивались в одну бесполезную issue.
func TestWrapLogger_CapturesBoundAndRecordAttrs(t *testing.T) {
	mt := enable(t)

	WrapLogger(discardLogger()).With(slog.String("slug", "internal-error")).
		Error("request failed", slog.String("request_id", "r-1"))

	events := mt.sent()
	require.Len(t, events, 1)
	require.Equal(t, "request failed", events[0].Message)
	attrs := events[0].Contexts["log_attrs"]
	require.Equal(t, "internal-error", attrs["slug"])
	require.Equal(t, "r-1", attrs["request_id"])
}

// Записи ниже Error в трекер не идут: он для ошибок, а не для потока логов.
func TestWrapLogger_IgnoresBelowError(t *testing.T) {
	mt := enable(t)

	log := WrapLogger(discardLogger())
	log.Info("обычная запись")
	log.Warn("предупреждение")

	require.Empty(t, mt.sent())
}

// Skip гасит дубль: паника уже ушла через CapturePanic, богаче.
func TestWrapLogger_SkipSuppressesCapture(t *testing.T) {
	mt := enable(t)

	WrapLogger(discardLogger()).LogAttrs(t.Context(), slog.LevelError, "panic recovered",
		slog.String("stack", "..."), Skip())

	require.Empty(t, mt.sent())
}

// При выключенном трекере обёртка прозрачна: уровни фильтруются, атрибуты
// доходят, запись не теряется.
func TestWrapLogger_PassthroughWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	log := WrapLogger(base)
	log.Debug("скрытая")
	log.Error("видимая", slog.String("k", "v"))

	require.NotContains(t, buf.String(), "скрытая")
	require.Contains(t, buf.String(), "видимая")
	require.Contains(t, buf.String(), "k=v")
	require.False(t, log.Enabled(t.Context(), slog.LevelDebug), "фильтрация уровня проходит сквозь обёртку")
}
