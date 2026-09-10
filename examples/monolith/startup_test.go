package monolith_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/smtp"
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
