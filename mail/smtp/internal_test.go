package smtp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gomail "github.com/wneessen/go-mail"
)

// Каждому режиму — своя политика go-mail; схлопнутые none и opportunistic
// обесценили бы AllowPlaintext.
func TestTLSPolicy_DistinctPerMode(t *testing.T) {
	t.Parallel()
	seen := make(map[gomail.TLSPolicy]TLSMode, len(AllTLSModes))
	for _, mode := range AllTLSModes {
		policy := tlsPolicy(mode)
		if prev, dup := seen[policy]; dup {
			t.Fatalf("режимы %q и %q дают одну политику %s", prev, mode, policy)
		}
		seen[policy] = mode
	}
	assert.Equal(t, gomail.TLSMandatory, tlsPolicy(TLSMandatory))
	assert.Equal(t, gomail.TLSOpportunistic, tlsPolicy(TLSOpportunistic))
	assert.Equal(t, gomail.NoTLS, tlsPolicy(TLSNone))
	assert.Equal(t, gomail.TLSMandatory, tlsPolicy("bogus"), "неизвестный режим — в сторону строгости")
}

// NOENC-варианты — только с AllowPlaintext.
func TestAuthType_NoEncOnlyWithAllowPlaintext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode  AuthMode
		allow bool
		want  gomail.SMTPAuthType
		ok    bool
	}{
		{AuthNone, false, "", false},
		{AuthNone, true, "", false},
		{AuthLogin, false, gomail.SMTPAuthLogin, true},
		{AuthLogin, true, gomail.SMTPAuthLoginNoEnc, true},
		{AuthPlain, false, gomail.SMTPAuthPlain, true},
		{AuthPlain, true, gomail.SMTPAuthPlainNoEnc, true},
	}
	for _, tc := range cases {
		got, ok := authType(tc.mode, tc.allow)
		assert.Equal(t, tc.ok, ok, "%s allow=%v", tc.mode, tc.allow)
		assert.Equal(t, tc.want, got, "%s allow=%v", tc.mode, tc.allow)
	}
	// Guard: у каждого режима из списка есть своя ветка.
	for _, mode := range AllAuthModes {
		_, ok := authType(mode, false)
		assert.Equal(t, mode != AuthNone, ok, "режим %q", mode)
	}
}

// Только формы с явным id; позиционный id Sendmail не угадывается.
func TestQueueID_KnownFormsOnly(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"2.0.0 Ok: queued as 4bXyZ123abc":                    "4bXyZ123abc",            // Postfix
		"2.0.0 Ok: queued as HE8QQrojjRkEzPiv9GWcjZ":         "HE8QQrojjRkEzPiv9GWcjZ", // Mailpit
		"OK id=1r8Xyz-000123-4A":                             "1r8Xyz-000123-4A",       // Exim
		"2.0.0 s85N3abc012345 Message accepted for delivery": "",                       // Sendmail
		"2.0.0 Ok": "",
		"":         "",
	}
	for response, want := range cases {
		assert.Equal(t, want, queueID(response), "ответ %q", response)
	}
}

// Пустые TLS и Auth — строгие умолчания.
func TestConfig_NormalizedDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Config{Host: "smtp.example.ru", Port: 587, Timeout: 1}.normalized()
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, TLSMandatory, cfg.TLS, "пустой TLS — mandatory, не opportunistic")
	assert.Equal(t, AuthNone, cfg.Auth)
}
