package monolith

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// TestRecipients_MatchLetter — Recipients отвечает на логин ровно так же, как
// сборка письма подтверждения: иначе session пропустил бы логин, на который
// письмо не соберётся, или отверг бы годный.
func TestRecipients_MatchLetter(t *testing.T) {
	lt := probeLetters(t)
	var accepted, rejected int
	for _, raw := range recipientSeeds() {
		switch agreeWithLetter(t, lt, raw) {
		case verdictAccepted:
			accepted++
		case verdictRejected:
			rejected++
		case verdictSkipped:
		}
	}
	// Корпус обязан задевать обе стороны: сравнение на одних годных ничего не доказывает.
	require.Positive(t, accepted, "в корпусе нет принятых адресов")
	require.Positive(t, rejected, "в корпусе нет отвергнутых адресов")
}

// FuzzRecipients_MatchLetter — то же свойство на произвольных строках.
func FuzzRecipients_MatchLetter(f *testing.F) {
	for _, seed := range recipientSeeds() {
		f.Add(seed)
	}
	lt := probeLetters(f)
	f.Fuzz(func(t *testing.T, raw string) {
		agreeWithLetter(t, lt, raw)
	})
}

// TestRegister_RejectionTextHasNoLogin — отказ Recipients у Register: класс
// loginid.ErrInvalid, причина mail в цепочке, личности нет, и логина в тексте
// ошибки нет — текст уходит в лог.
func TestRegister_RejectionTextHasNoLogin(t *testing.T) {
	ids := authtest.NewMemIdentities()
	svc := session.New(session.Deps{
		Identities: ids, Sessions: authtest.NewMemSessions(), Attempts: authtest.NewMemAttempts(),
		Tokens: authtest.NewMemTokens(ids), Hasher: newHasher(), Policy: newPolicy(),
		Notifier: authtest.NewRecordingNotifier(), Recipients: recipients{},
	}, session.DefaultConfig("shop", token.MustSecret([]byte(strings.Repeat("k", token.MinSecretLen)))))
	const login = "buyer.example.test"

	err := svc.Register(t.Context(), session.RegisterRequest{Login: login, Password: "q7-Lagoon-Quartz-Marmot-91"})

	require.ErrorIs(t, err, loginid.ErrInvalid)
	require.ErrorIs(t, err, mail.ErrInvalidMessage)
	require.NotContains(t, err.Error(), login)
	require.Zero(t, ids.Len(), "личность не заведена")
}

// verdict — чем кончилась сверка одного логина.
type verdict int

const (
	verdictSkipped verdict = iota
	verdictAccepted
	verdictRejected
)

// agreeWithLetter — Recipients и письмо подтверждения согласны о логине.
func agreeWithLetter(tb testing.TB, lt letters, raw string) verdict {
	tb.Helper()
	login, err := loginid.Normalize(raw)
	if err != nil {
		return verdictSkipped // до Recipients такой логин не доходит
	}
	checkErr := recipients{}.Check(login)
	_, _, letterErr := lt.letterFor(session.Notification{
		Realm: "shop", Kind: session.NotifyVerify, Login: login,
		RawToken: "probe-token", ExpiresAt: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	if (checkErr == nil) != (letterErr == nil) {
		tb.Errorf("логин %q: Recipients — %v, письмо — %v", login, checkErr, letterErr)
	}
	if checkErr == nil {
		return verdictAccepted
	}
	return verdictRejected
}

// probeLetters — сборщик писем на настоящем mail.Service с политикой сборки.
func probeLetters(tb testing.TB) letters {
	tb.Helper()
	svc := mail.NewService(mailtest.NewMemStore(), mailtest.NewTransport(), nil,
		mailConfig(Config{MailFrom: "shop@example.test", MailDomain: "example.test"}))
	return letters{svc: svc, baseURL: "https://shop.example.test"}
}

// recipientSeeds — корпус: принятые и отвергнутые формы адреса.
func recipientSeeds() []string {
	return []string{
		"buyer@example.test",
		"  Buyer@Example.TEST  ",
		"buyer+tag@example.test",
		"buyer.example.test",
		"buyer@",
		"@example.test",
		"buyer @example.test",
		"buyer @example.test",
		"<buyer@example.test>",
		"buyer@example.test,other@example.test",
		"buyer;x@example.test",
		`"quoted local"@example.test`,
		"buyer@[127.0.0.1]",
		"покупатель@пример.тест",
		"buyer＠example.test",
		"buyer@example.test.",
		"a@b@example.test",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", 184) + ".test",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", 185) + ".test",
	}
}
