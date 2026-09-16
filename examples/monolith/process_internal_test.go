package monolith

import (
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
