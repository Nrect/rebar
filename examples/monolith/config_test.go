package monolith_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/smtp"

	"github.com/nrect/rebar/examples/monolith"
)

// TestLoad_DefaultsAreSafe — умолчания годятся для прода: забытая переменная
// предохранитель не снимает, послабления стенда ставит stand.env словом.
func TestLoad_DefaultsAreSafe(t *testing.T) {
	cfg, err := monolith.Load(loaderOf(prodEnv(), nil))
	require.NoError(t, err)

	require.True(t, cfg.CookieSecure, "SESSION_COOKIE_SECURE")
	require.Equal(t, smtp.TLSMandatory, cfg.SMTP.TLS, "SMTP_TLS")
	require.Equal(t, smtp.AuthPlain, cfg.SMTP.Auth, "SMTP_AUTH")
	require.False(t, cfg.SMTP.AllowPlaintext, "SMTP_ALLOW_PLAINTEXT")
	require.Equal(t, "127.0.0.1:9090", cfg.InternalAddr, "служебный порт наружу открывают явно")
	require.Equal(t, "info", cfg.LogLevel)
}

// TestLoad_ProdValuesHaveNoDefault — того, без чего прод неверен, в коде нет:
// старт падает списком, и забытые переменные чинятся одним перезапуском.
func TestLoad_ProdValuesHaveNoDefault(t *testing.T) {
	_, err := monolith.Load(loaderOf(map[string]string{}, nil))
	require.Error(t, err)
	for _, key := range []string{
		"ENVIRONMENT", "AUTH_SECRET", "DATABASE_URL", "BASE_URL",
		"MAIL_FROM", "MAIL_DOMAIN", "SMTP_HOST", "SMTP_AUTH",
	} {
		require.ErrorContains(t, err, key+":")
	}
}

// TestLoad_SMTPLoginNeedsCredentials — вход без пароля отвергается в том же
// списке, а не отдельным перезапуском из smtp.New; relay без входа — словом.
func TestLoad_SMTPLoginNeedsCredentials(t *testing.T) {
	env := prodEnv()
	delete(env, "SMTP_PASSWORD")
	_, err := monolith.Load(loaderOf(env, nil))
	require.ErrorContains(t, err, "SMTP_AUTH:")

	env = prodEnv()
	delete(env, "SMTP_USER")
	delete(env, "SMTP_PASSWORD")
	_, err = monolith.Load(loaderOf(env, map[string]string{"SMTP_AUTH": "none"}))
	require.NoError(t, err)
}

// prodEnv — окружение прода без послаблений: только то, чего в коде нет.
func prodEnv() map[string]string {
	return map[string]string{
		"ENVIRONMENT":   "production",
		"AUTH_SECRET":   testSecret,
		"DATABASE_URL":  "postgres://shop@db:5432/shop",
		"BASE_URL":      "https://shop.example.com",
		"MAIL_FROM":     "shop@example.com",
		"MAIL_DOMAIN":   "example.com",
		"SMTP_HOST":     "smtp.example.com",
		"SMTP_USER":     "shop",
		"SMTP_PASSWORD": "пароль-почтовика",
	}
}
