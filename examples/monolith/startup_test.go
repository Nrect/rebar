package monolith_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/smtp"
	"github.com/nrect/rebar/payment/paymenttest"
)

// TestStart_RefusesBadTransportConfig — опечатка в настройках почты роняет
// СБОРКУ, а не первое письмо.
//
// smtp.New к сети не ходит, поэтому его единственная ошибка — негодный
// конфиг, и терпеть её нечем: стартовавшее приложение, которое не может
// отправить письмо подтверждения, — это сломанная регистрация с зелёным
// healthz. Письма копились бы в email_outbox, а узнали бы мы об этом от
// гейджа возраста старейшего pending — если бы кто-то за ним смотрел.
//
// Страж, а не украшение: до этого теста ошибка проглатывалась, и подстановка
// mail.Unconfigured выглядела в коде безобидно.
func TestStart_RefusesBadTransportConfig(t *testing.T) {
	// TLS выключен, а послабление для незашифрованного соединения не выдано:
	// классическая опечатка стенда, уехавшая в прод.
	_, _, err := tryBuildApp(t, map[string]string{
		"SMTP_TLS":             string(smtp.TLSNone),
		"SMTP_ALLOW_PLAINTEXT": "false",
	})
	require.Error(t, err, "приложение не должно стартовать с негодным конфигом почты")
	require.ErrorIs(t, err, smtp.ErrInvalidConfig)
}

// TestStart_SnapshotsGaugesBeforeFirstTick — гейджи отражают снимок ДО первого
// такта задачи: App.Start снимает их сразу.
//
// Минута нулей после старта — минута, когда настоящее расхождение книг
// невидимо: payment_drift кормит денежный алерт с порогом 1, а приходится эта
// минута ровно на момент после деплоя. Такт в тесте — час, так что единица в
// гейдже может взяться только из прогона при старте.
func TestStart_SnapshotsGaugesBeforeFirstTick(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))

	// Намерение, оставшееся created после временного сбоя провайдера и
	// состаренное на час: сверка считает его зависшим.
	s.app.Provider().SetCreateErr(paymenttest.ErrProviderDown)
	requireRefusal(t, s, "startup", http.StatusServiceUnavailable, "payment-unavailable")
	s.app.Provider().SetCreateErr(nil)
	_, err := s.pool(t).Exec(t.Context(),
		`UPDATE payment_intents SET created_at = created_at - interval '1 hour'
		 WHERE status = 'created'`)
	require.NoError(t, err)

	// До старта снимка нет: зависшее уже лежит, а гейдж отдаёт ноль.
	requireMetric(t, s.scrape(t), "payment_intents_stuck", nil, 0)

	require.NoError(t, s.app.Start(t.Context()))
	t.Cleanup(s.app.Jobs().Stop)

	requireMetric(t, s.scrape(t), "payment_intents_stuck", nil, 1)
}
