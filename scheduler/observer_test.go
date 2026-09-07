package scheduler_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/scheduler"
)

func logTo(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// syncBuffer — буфер под замком: в логгер пишут горутины задач, а читает его
// тест, и -race поймал бы голый bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Успешный прогон — Debug: фоновая задача, отработавшая как задумано, не
// новость, а на интервале в секунды она затопила бы info-лог.
func TestLogObserver_SuccessIsDebug(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	obs := scheduler.LogObserver(logTo(&buf))
	obs.Finished(context.Background(), scheduler.Run{Job: "mail_deliver", Processed: 3, Elapsed: time.Second})

	out := buf.String()
	assert.Contains(t, out, `"level":"DEBUG"`)
	assert.Contains(t, out, "mail_deliver")
	assert.Contains(t, out, `"processed":3`)
}

// Ошибка и паника — Error с флагом panicked: по нему разбор отличает баг в
// коде от отказа внешней системы.
func TestLogObserver_FailureIsError(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	obs := scheduler.LogObserver(logTo(&buf))
	obs.Finished(context.Background(), scheduler.Run{
		Job: "mail_deliver", Err: errRun, Panicked: false,
	})
	obs.Finished(context.Background(), scheduler.Run{
		Job: "mail_purge", Err: scheduler.ErrPanic, Panicked: true,
	})

	out := buf.String()
	assert.Contains(t, out, `"level":"ERROR"`)
	assert.Contains(t, out, errRun.Error())
	assert.Contains(t, out, `"panicked":true`)
	assert.NotContains(t, out, `"level":"DEBUG"`)
}

// Started пишет имена задач: по этой строке в логе видно, что именно
// запустилось в этой сборке.
func TestLogObserver_StartedNamesJobs(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	obs := scheduler.LogObserver(logTo(&buf))
	obs.Started([]string{"mail_deliver", "mail_purge"}, time.Now())

	assert.Contains(t, buf.String(), "mail_purge")
}

// Nil-логгер — не паника: логгер не порт, и slog.Default() у процесса есть
// всегда. Проверяется именно живучесть, а не адрес назначения.
func TestLogObserver_NilLoggerUsesDefault(t *testing.T) {
	t.Parallel()

	obs := scheduler.LogObserver(nil)
	require.NotNil(t, obs)
	assert.NotPanics(t, func() {
		obs.Started([]string{"mail_deliver"}, time.Now())
		obs.Finished(context.Background(), scheduler.Run{Job: "mail_deliver"})
	})
}

// Планировщик собирается с LogObserver и работает: это путь потребителя без
// метрик, и он обязан быть живым, а не только компилируемым.
func TestLogObserver_DrivesScheduler(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	done := make(chan struct{}, 1)
	s := newSched(t, scheduler.LogObserver(logTo(&buf)), scheduler.Job{
		Name: "mail_deliver", Interval: tick,
		Run: func(context.Context) (int, error) {
			select {
			case done <- struct{}{}:
			default:
			}
			return 1, nil
		},
	})
	started(t, s)

	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("задача не выполнилась")
	}
	assert.Contains(t, buf.String(), "scheduler started")
}
