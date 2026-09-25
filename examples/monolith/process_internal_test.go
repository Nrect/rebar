package monolith

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sendEstimate — обычная отправка письма через SMTP провайдера. На этой оценке
// держится бюджет задач: провайдер медленнее — пачку укорачивают.
const sendEstimate = 250 * time.Millisecond

// handlerEstimate — обычный хендлер outbox: заказ и адрес из базы, письмо в
// очередь. Хендлер медленнее — пачку укорачивают.
const handlerEstimate = 100 * time.Millisecond

// TestStopBudgets_FitKillDeadline — бюджеты остановки по docs/CONSUMER.md, §5:
// сумма меньше 30 с Kubernetes, а задачам хватает обычной пачки mail и двух
// SendTimeout, так же — пачки outbox и двух HandlerTimeout, на запись исхода и
// возврат остатка. Задачи идут каждая в своей горутине, поэтому бюджет
// покрывает каждую, а не сумму. Поднятые BatchSize или сроки без бюджета
// оставили бы пачку под арендой на каждом выкате.
func TestStopBudgets_FitKillDeadline(t *testing.T) {
	require.Less(t, httpGrace+jobsGrace+flushGrace, 30*time.Second, "сумма бюджетов")

	mc := mailConfig(Config{})
	mailBatch := time.Duration(mc.BatchSize) * (sendEstimate + mc.MinSendGap)
	require.GreaterOrEqual(t, jobsGrace, mailBatch+2*mc.SendTimeout,
		"jobsGrace не покрывает пачку mail и два SendTimeout")

	oc := outboxConfig()
	outboxBatch := time.Duration(oc.BatchSize) * handlerEstimate
	require.GreaterOrEqual(t, jobsGrace, outboxBatch+2*oc.HandlerTimeout,
		"jobsGrace не покрывает пачку outbox и два HandlerTimeout")
}

// slowUplink — медленный мобильный канал вверх, 512 кбит/с, в байтах за секунду.
const slowUplink = 64_000

// lbIdle — простой соединения у балансировщика перед процессом.
const lbIdle = 60 * time.Second

// TestServerTimeouts_FitBudgets — сроки соединения по docs/CONSUMER.md, §9,
// п. 1: тело JSON на медленном канале укладывается в readTimeout, файл — нет,
// поэтому ручка загрузки продлевает срок, и продления хватает; ручке хватает
// writeTimeout на запрос к базе; keep-alive закрывает балансировщик, а не сервер.
func TestServerTimeouts_FitBudgets(t *testing.T) {
	transfer := func(n int) time.Duration { return time.Duration(n) * time.Second / slowUplink }

	require.Less(t, readHeaderTimeout, readTimeout, "заголовки дольше запроса целиком")
	require.Less(t, transfer(maxJSONBytes), readTimeout, "тело JSON на медленном канале не укладывается в readTimeout")
	require.Greater(t, transfer(maxUploadBytes), readTimeout, "файл укладывается в readTimeout: продление срока лишнее")
	require.Less(t, transfer(maxUploadBytes), uploadTimeout, "файл на медленном канале не укладывается в uploadTimeout")
	require.Greater(t, writeTimeout, poolConfig().StatementTimeout, "writeTimeout не покрывает statement_timeout пула")
	require.Greater(t, idleTimeout, lbIdle, "сервер рвёт keep-alive раньше балансировщика")
}

// panicListener — Accept падает паникой: так паникует Serve.
type panicListener struct{ net.Listener }

func (panicListener) Accept() (net.Conn, error) { panic("accept упал") }

// TestServe_PanicBecomesOwnerError — паника Serve уходит ошибкой владельцу, а не
// роняет процесс мимо остановки.
func TestServe_PanicBecomesOwnerError(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	require.NotPanics(t, func() {
		serve(served, newServer("test", http.NotFoundHandler()), panicListener{ln})
	}, "паника Serve вышла из serve мимо владельца")
	select {
	case err := <-served:
		require.ErrorContains(t, err, "accept упал", "паника Serve не дошла до владельца")
	default:
		t.Fatal("serve вернулся, ничего не отдав владельцу")
	}
}

// TestWithin_PanicIsError — within отдаёт исход stop: паника — ошибкой,
// вышедший бюджет — errOutlivedBudget.
func TestWithin_PanicIsError(t *testing.T) {
	require.NoError(t, within(func() {}, time.Second))
	require.ErrorContains(t, within(func() { panic("stop упал") }, time.Second), "stop упал")

	release := make(chan struct{})
	defer close(release)
	require.ErrorIs(t, within(func() { <-release }, 10*time.Millisecond), errOutlivedBudget)
}
