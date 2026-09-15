package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel модуля, подпакеты включительно, несёт класс
// или отказ от него с доводом (ADR-0007). Двойники в allow: своего класса у их
// sentinel нет — класс приходит обёрткой (ADR-0007, «Двойники»).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "authtest")
}

type sentinel struct {
	name   string
	err    error
	kind   errs.Kind
	prefix string
}

func sentinels() []sentinel {
	return []sentinel{
		{"auth.ErrInvalidRealm", auth.ErrInvalidRealm, errs.KindUnknown, "auth: "},
		{"auth.ErrIdentityNotFound", auth.ErrIdentityNotFound, errs.KindUnknown, "auth: "},
		{"auth.ErrLoginTaken", auth.ErrLoginTaken, errs.KindConflict, "auth: "},
		{"auth.ErrUnavailable", auth.ErrUnavailable, errs.KindUnavailable, "auth: "},
		{"authhttp.ErrCSRF", authhttp.ErrCSRF, errs.KindForbidden, "authhttp: "},
		{"loginid.ErrInvalid", loginid.ErrInvalid, errs.KindIncorrectInput, "loginid: "},
		{"password.ErrBusy", password.ErrBusy, errs.KindTooManyRequests, "password: "},
		{"password.ErrHashInvalid", password.ErrHashInvalid, errs.KindUnknown, "password: "},
		{"password.ErrTooShort", password.ErrTooShort, errs.KindIncorrectInput, "password: "},
		{"password.ErrTooLong", password.ErrTooLong, errs.KindIncorrectInput, "password: "},
		{"password.ErrTooWeak", password.ErrTooWeak, errs.KindIncorrectInput, "password: "},
		{"session.ErrInvalidCredentials", session.ErrInvalidCredentials, errs.KindUnauthenticated, "session: "},
		{"session.ErrTooManyAttempts", session.ErrTooManyAttempts, errs.KindTooManyRequests, "session: "},
		{"session.ErrNotVerified", session.ErrNotVerified, errs.KindForbidden, "session: "},
		{"session.ErrNoSession", session.ErrNoSession, errs.KindUnauthenticated, "session: "},
		{"session.ErrTokenInvalid", session.ErrTokenInvalid, errs.KindIncorrectInput, "session: "},
		{"token.ErrSecretTooShort", token.ErrSecretTooShort, errs.KindUnknown, "token: "},
	}
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через errors.Is.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range sentinels() {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), tc.prefix), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Внутри модуля префикс не различает: две sentinel одного пакета с одним
// классом и текстом были бы взаимозаменяемы через errors.Is.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	all := sentinels()
	for i, a := range all {
		for j, b := range all {
			if i != j {
				assert.NotErrorIsf(t, a.err, b.err, "%s совпала с %s через errors.Is", a.name, b.name)
			}
		}
	}
}
