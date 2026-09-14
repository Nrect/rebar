package session_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/kit/errs"
)

// errPortDown — причина без класса, как её отдаёт голый двойник.
var errPortDown = errors.New("connection refused")

// Сбой порта на путях из запроса и из планировщика доходит до вызывающего с
// классом 503, а не голой причиной двойника: класс несёт обёртка ядра.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	// Порты падают разом: каждый метод встречает первый сбой на своём пути.
	t.Run("все порты разом", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)
		id := st.seed(t, knownLogin)
		st.ids.SetErr(errPortDown)
		st.sessions.SetErr(errPortDown)
		st.attempts.SetErr(errPortDown)
		st.tokens.SetErr(errPortDown)
		ctx := t.Context()

		assertUnavailable(t, st.svc.Register(ctx, session.RegisterRequest{Login: unknownLogin, Password: goodPassword}), "Register")
		_, err := st.signIn(t, knownLogin, goodPassword)
		assertUnavailable(t, err, "SignIn")
		_, err = st.svc.Resolve(ctx, "raw-token")
		assertUnavailable(t, err, "Resolve")
		assertUnavailable(t, st.svc.SignOut(ctx, "raw-token"), "SignOut")
		_, err = st.svc.SignOutAll(ctx, id.ID)
		assertUnavailable(t, err, "SignOutAll")
		assertUnavailable(t, st.svc.ChangePassword(ctx, session.ChangePasswordRequest{
			SubjectID: id.ID, Current: goodPassword, New: wrongPassword,
		}), "ChangePassword")
		assertUnavailable(t, st.svc.RequestVerification(ctx, knownLogin), "RequestVerification")
		assertUnavailable(t, st.svc.RequestReset(ctx, knownLogin), "RequestReset")
		assertUnavailable(t, st.svc.RequestEmailChange(ctx, session.EmailChangeRequest{
			SubjectID: id.ID, NewLogin: unknownLogin, Password: goodPassword,
		}), "RequestEmailChange")
		_, err = st.svc.ConfirmVerification(ctx, "raw-token")
		assertUnavailable(t, err, "ConfirmVerification")
		assertUnavailable(t, st.svc.ConfirmReset(ctx, "raw-token", goodPassword), "ConfirmReset")
		_, err = st.svc.ConfirmEmailChange(ctx, "raw-token")
		assertUnavailable(t, err, "ConfirmEmailChange")
		_, err = st.svc.Sweep(ctx)
		assertUnavailable(t, err, "Sweep")
	})

	// Места обёртки глубже первого порта на пути: падает только нужный порт.
	t.Run("SignIn: личность, попытка, сессия", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)
		st.seed(t, knownLogin)

		st.ids.SetErr(errPortDown)
		_, err := st.signIn(t, knownLogin, goodPassword)
		assertUnavailable(t, err, "lookup identity")
		st.ids.SetErr(nil)

		st.attempts.SetRecordErr(errPortDown)
		_, err = st.signIn(t, knownLogin, wrongPassword)
		assertUnavailable(t, err, "record attempt")
		st.attempts.SetRecordErr(nil)

		st.sessions.SetErr(errPortDown)
		_, err = st.signIn(t, knownLogin, goodPassword)
		assertUnavailable(t, err, "insert session")
	})

	t.Run("Resolve: продление и личность", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)
		st.seed(t, knownLogin)
		res, err := st.signIn(t, knownLogin, goodPassword)
		require.NoError(t, err)
		st.clock.Advance(st.cfg.RenewEvery)

		st.sessions.SetTouchErr(errPortDown)
		_, err = st.svc.Resolve(t.Context(), res.Token)
		assertUnavailable(t, err, "touch session")

		st.ids.SetErr(errPortDown)
		_, err = st.svc.Resolve(t.Context(), res.Token)
		assertUnavailable(t, err, "lookup identity")
	})

	t.Run("Register: выдача токена", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)
		st.tokens.SetErr(errPortDown)

		err := st.svc.Register(t.Context(), session.RegisterRequest{Login: unknownLogin, Password: goodPassword})
		assertUnavailable(t, err, "issue token")
	})

	t.Run("ChangePassword: отзыв и запись хэша", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)
		id := st.seed(t, knownLogin)
		change := session.ChangePasswordRequest{SubjectID: id.ID, Current: goodPassword, New: wrongPassword}

		st.sessions.SetErr(errPortDown)
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "revoke sessions")
		st.sessions.SetErr(nil)

		st.tokens.SetErr(errPortDown)
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "revoke reset tokens")
		st.tokens.SetErr(nil)

		st.ids.SetPasswordErr = errPortDown
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "set password hash")
	})

	t.Run("Sweep: попытки и токены", func(t *testing.T) {
		t.Parallel()
		st := newStand(t)

		st.attempts.SetErr(errPortDown)
		_, err := st.svc.Sweep(t.Context())
		assertUnavailable(t, err, "sweep attempts")
		st.attempts.SetErr(nil)

		st.tokens.SetErr(errPortDown)
		_, err = st.svc.Sweep(t.Context())
		assertUnavailable(t, err, "sweep tokens")
	})
}

// assertUnavailable — класс 503 и причина двойника в цепочке: обёртка не
// потеряла ни класса, ни исходной ошибки.
func assertUnavailable(t *testing.T, err error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	assert.ErrorIsf(t, err, errPortDown, "причина двойника на %s", site)
}
