package monolith_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// reconcileJob — задача под pglock (jobs.go).
const reconcileJob = "payments_reconcile"

// signalWait — потолок ожидания сигнала: сломанная проводка падает по нему, а
// не висит до таймаута бинаря.
const signalWait = 30 * time.Second

type runOutcome struct {
	processed int
	err       error
}

// TestPaymentsReconcile_OneReplicaRuns — две реплики на одной базе: сверку
// выполняет одна, вторая пропускает, и пропуск виден в cron_lock_total.
//
// ЛОВУШКА ADR-0008 ПРОВЕРЯЕТСЯ ЗДЕСЬ ЖЕ: планировщик второй реплики записал
// пропуск успехом (cron_runs_total{result="ok"}), и отличить его от прогона
// может только счётчик блокировки.
func TestPaymentsReconcile_OneReplicaRuns(t *testing.T) {
	a := newStand(t)
	signIn(t, a, registerAndConfirm(t, a))
	unstartedIntent(t, a)
	b := newReplica(t, a)

	// Ряды рождаются нулём ещё до первой попытки.
	for _, result := range []string{"acquired", "skipped", "error"} {
		requireMetric(t, b.scrape(t), "cron_lock_total", lockSeries(result), 0)
	}

	createdBefore := len(a.app.Provider().Created())
	entered, release := holdCreatePayment(t, a)
	done := make(chan runOutcome, 1)
	go func() {
		processed, err := a.app.Jobs().RunNow(context.WithoutCancel(t.Context()), reconcileJob)
		done <- runOutcome{processed: processed, err: err}
	}()
	waitClosed(t, entered, "реплика A взяла ключ и дошла до провайдера")

	processed, err := b.app.Jobs().RunNow(t.Context(), reconcileJob)
	require.NoError(t, err, "ключ у соседа — не ошибка")
	require.Zero(t, processed, "реплика B сверку не выполняла")

	release()
	var ranA runOutcome
	select {
	case ranA = <-done:
	case <-time.After(signalWait):
		t.Fatal("сверка реплики A не закончилась")
	}
	require.NoError(t, ranA.err, "сверка реплики A")
	require.Equal(t, 1, ranA.processed, "реплика A разобрала намерение")

	// Эффект один: сверка позвала провайдера один раз на две реплики.
	require.Len(t, a.app.Provider().Created(), createdBefore+1, "сверка A создала платёж")
	require.Empty(t, b.app.Provider().Created(), "реплика B до провайдера не дошла")
	requireCount(t, a, 1, "SELECT count(*) FROM payment_intents WHERE status = 'pending'")

	bodyA, bodyB := a.scrape(t), b.scrape(t)
	requireMetric(t, bodyA, "cron_lock_total", lockSeries("acquired"), 1)
	requireMetric(t, bodyA, "cron_lock_total", lockSeries("skipped"), 0)
	requireMetric(t, bodyB, "cron_lock_total", lockSeries("skipped"), 1)
	requireMetric(t, bodyB, "cron_lock_total", lockSeries("acquired"), 0)
	requireMetric(t, bodyB, "cron_runs_total",
		map[string]string{"job": reconcileJob, "result": "ok"}, 1)
}

// unstartedIntent — намерение без платежа у провайдера, зависшее для сверки:
// провайдер не ответил на checkout, строка осталась created, и её состарили на
// час. TTL не тронут, поэтому сверка дозавершает её походом к провайдеру.
func unstartedIntent(t *testing.T, s *stand) {
	t.Helper()
	s.app.Provider().SetCreateErr(paymenttest.ErrProviderDown)
	requireRefusal(t, s, "replicas", http.StatusServiceUnavailable, "payment-unavailable")
	s.app.Provider().SetCreateErr(nil)
	_, err := s.pool(t).Exec(t.Context(),
		`UPDATE payment_intents SET created_at = created_at - interval '1 hour'
		 WHERE status = 'created'`)
	require.NoError(t, err)
	requireCount(t, s, 1, "SELECT count(*) FROM payment_intents WHERE status = 'created'")
}

// holdCreatePayment задерживает сверку реплики внутри похода к провайдеру:
// ключ в этот момент у неё. Отпускается и из Cleanup — упавший тест не оставит
// прогон висеть с соединением пула.
func holdCreatePayment(t *testing.T, s *stand) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	enter := sync.OnceFunc(func() { close(in) })
	release = sync.OnceFunc(func() { close(out) })
	t.Cleanup(release)
	s.app.Provider().SetCreateHook(func(payment.CreatePaymentRequest) {
		enter()
		<-out
	})
	return in, release
}

// waitClosed ждёт закрытия канала не дольше signalWait.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(signalWait):
		t.Fatalf("не дождались: %s", what)
	}
}

// lockSeries — метки ряда cron_lock_total задачи сверки.
func lockSeries(result string) map[string]string {
	return map[string]string{"job": reconcileJob, "result": result}
}
