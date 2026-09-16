package monolith_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStop_RunningJobWritesOutcome — сигнал не режет идущий прогон: остановка
// ждёт его между прогонами, и прогон дописывает исход в базу.
//
// Прогон — настоящая сверка платежей по расписанию, задержанная внутри похода
// к провайдеру: сигнал приходит, когда платёж у провайдера уже создан, а исход
// ещё не записан. Дойди сигнал до контекста задач — запись упала бы на
// отменённом контексте, и у созданного платежа осталось бы намерение created.
func TestStop_RunningJobWritesOutcome(t *testing.T) {
	s := newStandWith(t, map[string]string{"TICK": "20ms"})
	signIn(t, s, registerAndConfirm(t, s))
	unstartedIntent(t, s)
	createdBefore := len(s.app.Provider().Created())
	entered, release := holdCreatePayment(t, s)

	signal, sigterm := startProcess(t, s)
	waitClosed(t, entered, "сверка по расписанию дошла до провайдера")

	stopped := make(chan error, 1)
	go func() { stopped <- s.app.Wait(signal) }()
	sigterm()

	_, internal := s.app.Addrs()
	waitStatus(t, "http://"+internal+"/readyz", http.StatusServiceUnavailable, "остановка началась: /readyz → 503")
	select {
	case err := <-stopped:
		t.Fatalf("остановка не дождалась идущего прогона: %v", err)
	default:
	}

	release()
	select {
	case err := <-stopped:
		require.NoError(t, err, "остановка")
	case <-time.After(signalWait):
		t.Fatal("остановка не закончилась после прогона")
	}

	require.Len(t, s.app.Provider().Created(), createdBefore+1, "платёж у провайдера создан до сигнала")
	requireCount(t, s, 1, "SELECT count(*) FROM payment_intents WHERE status = 'pending'")
	for _, rec := range s.logs.records(t, "cron job failed") {
		require.NotEqual(t, reconcileJob, rec["job"], "прогон сверки упал на остановке: %v", rec["error"])
	}
}

// TestReadyz_SchemaMismatchIs503 — /readyz сверяет схему блоков тем же
// списком, что и старт: выкат, чьей схемы в базе нет, останавливается на
// пробе, а не на запросах. Служебные ручки — только на служебном порту.
func TestReadyz_SchemaMismatchIs503(t *testing.T) {
	s := newStand(t)
	startProcess(t, s)
	public, internal := s.app.Addrs()

	require.Equal(t, http.StatusOK, statusOf(t, "http://"+internal+"/readyz"), "процесс готов, схема сходится")
	require.Equal(t, http.StatusOK, statusOf(t, "http://"+internal+"/healthz"))
	require.Equal(t, http.StatusOK, statusOf(t, "http://"+internal+"/metrics"))
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		require.Equal(t, http.StatusNotFound, statusOf(t, "http://"+public+path), "%s на публичном порту", path)
	}

	// Колонки, которую ждёт код mail, в базе больше нет.
	_, err := s.pool(t).Exec(t.Context(),
		`ALTER TABLE email_outbox RENAME COLUMN provider_message_id TO provider_message_ref`)
	require.NoError(t, err)

	require.Equal(t, http.StatusServiceUnavailable, statusOf(t, "http://"+internal+"/readyz"),
		"схема mail не сходится — трафик не шлют")
	require.Equal(t, http.StatusOK, statusOf(t, "http://"+internal+"/healthz"), "liveness в базу не ходит")

	recs := s.logs.records(t, "not ready")
	require.Len(t, recs, 1)
	require.Equal(t, "WARN", recs[0]["level"], "проба повторяется: Error засыпал бы трекер")
	require.Equal(t, "readyz", recs[0]["op"])
	require.Contains(t, recs[0]["error"], "provider_message_id", "причина — в лог, а не в ответ")
}

// TestStart_BusyPortRefuses — занятый порт — отказ Start, а не ошибка в
// горутине после того, как процесс объявил себя готовым.
func TestStart_BusyPortRefuses(t *testing.T) {
	for _, key := range []string{"ADDR", "INTERNAL_ADDR"} {
		t.Run(key, func(t *testing.T) {
			var lc net.ListenConfig
			busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = busy.Close() })

			s := newStandWith(t, map[string]string{key: busy.Addr().String()})
			err = s.app.Start(t.Context())
			require.Error(t, err, "порт %s занят — старт обязан отказать", key)
			require.ErrorContains(t, err, busy.Addr().String())
		})
	}
}

// startProcess запускает процесс стенда на портах :0 и отдаёт его сигнальный
// контекст с отменой — она и есть SIGTERM. Остановка — ещё и в Cleanup: Stop
// идемпотентен.
func startProcess(t *testing.T, s *stand) (signal context.Context, sigterm context.CancelFunc) {
	t.Helper()
	signal, sigterm = context.WithCancel(t.Context())
	t.Cleanup(sigterm)
	require.NoError(t, s.app.Start(signal), "старт процесса")
	t.Cleanup(func() { _ = s.app.Stop(signal) })
	return signal, sigterm
}

// statusOf — статус GET без кук.
func statusOf(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// waitStatus опрашивает url, пока он не ответит want, не дольше signalWait.
func waitStatus(t *testing.T, url string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(signalWait)
	for statusOf(t, url) != want {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
