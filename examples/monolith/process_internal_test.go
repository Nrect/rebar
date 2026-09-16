package monolith

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sendEstimate — обычная отправка письма через SMTP провайдера. На этой оценке
// держится бюджет задач: провайдер медленнее — пачку укорачивают.
const sendEstimate = 250 * time.Millisecond

// TestStopBudgets_FitKillDeadline — бюджеты остановки по docs/CONSUMER.md, §5:
// сумма меньше 30 с Kubernetes, а задачам хватает обычной пачки mail и двух
// SendTimeout на запись исхода и возврат остатка. Поднятые BatchSize или
// SendTimeout без бюджета оставили бы пачку под арендой на каждом выкате.
func TestStopBudgets_FitKillDeadline(t *testing.T) {
	require.Less(t, httpGrace+jobsGrace+flushGrace, 30*time.Second, "сумма бюджетов")

	mc := mailConfig(Config{})
	batch := time.Duration(mc.BatchSize) * (sendEstimate + mc.MinSendGap)
	require.GreaterOrEqual(t, jobsGrace, batch+2*mc.SendTimeout,
		"jobsGrace не покрывает пачку mail и два SendTimeout")
}
