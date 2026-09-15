package session_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/kit/errs"
)

// errPortDown — причина без класса: её отдают голые заглушки портов ниже и
// двойники портов, которые пишет потребитель.
var errPortDown = errors.New("connection refused")

// Сбой порта на путях из запроса и из планировщика доходит до вызывающего с
// классом 503: класс несёт обёртка ядра. Сессии и попытки здесь идут через
// голые заглушки: двойники authtest заворачивают сбой сами, как authpg, и
// снятой обёртки ядра страж бы не увидел. Личности и токены пишет потребитель,
// их двойники отдают сбой голым.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	// Порты падают разом: каждый метод встречает первый сбой на своём пути.
	t.Run("все порты разом", func(t *testing.T) {
		t.Parallel()
		st, sessions, attempts := newBareStand(t)
		id := st.seed(t, knownLogin)
		st.ids.SetErr(errPortDown)
		sessions.down.Store(true)
		attempts.down.Store(true)
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
		st, sessions, attempts := newBareStand(t)
		st.seed(t, knownLogin)

		st.ids.SetErr(errPortDown)
		_, err := st.signIn(t, knownLogin, goodPassword)
		assertUnavailable(t, err, "lookup identity")
		st.ids.SetErr(nil)

		attempts.recordDown.Store(true)
		_, err = st.signIn(t, knownLogin, wrongPassword)
		assertUnavailable(t, err, "record attempt")
		attempts.recordDown.Store(false)

		sessions.down.Store(true)
		_, err = st.signIn(t, knownLogin, goodPassword)
		assertUnavailable(t, err, "insert session")
	})

	t.Run("Resolve: продление и личность", func(t *testing.T) {
		t.Parallel()
		st, sessions, _ := newBareStand(t)
		st.seed(t, knownLogin)
		res, err := st.signIn(t, knownLogin, goodPassword)
		require.NoError(t, err)
		st.clock.Advance(st.cfg.RenewEvery)

		sessions.touchDown.Store(true)
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
		st, sessions, _ := newBareStand(t)
		id := st.seed(t, knownLogin)
		change := session.ChangePasswordRequest{SubjectID: id.ID, Current: goodPassword, New: wrongPassword}

		sessions.down.Store(true)
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "revoke sessions")
		sessions.down.Store(false)

		st.tokens.SetErr(errPortDown)
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "revoke reset tokens")
		st.tokens.SetErr(nil)

		st.ids.SetPasswordErr = errPortDown
		assertUnavailable(t, st.svc.ChangePassword(t.Context(), change), "set password hash")
	})

	t.Run("Sweep: попытки и токены", func(t *testing.T) {
		t.Parallel()
		st, _, attempts := newBareStand(t)

		attempts.down.Store(true)
		_, err := st.svc.Sweep(t.Context())
		assertUnavailable(t, err, "sweep attempts")
		attempts.down.Store(false)

		st.tokens.SetErr(errPortDown)
		_, err = st.svc.Sweep(t.Context())
		assertUnavailable(t, err, "sweep tokens")
	})
}

// assertUnavailable — класс 503 и причина в цепочке: обёртка не потеряла ни
// класса, ни исходной ошибки.
func assertUnavailable(t *testing.T, err error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	assert.ErrorIsf(t, err, errPortDown, "причина на %s", site)
}

// newBareStand — стенд newStand, но сессии и попытки сервис берёт через голые
// заглушки поверх тех же двойников: пока сбой не включён, вызов уходит в
// двойник.
func newBareStand(t *testing.T) (*stand, *bareSessions, *bareAttempts) {
	t.Helper()
	st := newStand(t)
	sessions := &bareSessions{MemSessions: st.sessions}
	attempts := &bareAttempts{MemAttempts: st.attempts}
	deps := st.deps()
	deps.Sessions, deps.Attempts = sessions, attempts
	st.svc = session.New(deps, st.cfg)
	st.svc.SetClock(st.clock.Now)
	return st, sessions, attempts
}

// bareSessions — session.Sessions, отдающий errPortDown голым, пока сбой
// включён. Флаги атомарные: сервис вправе звать порт из своих горутин.
type bareSessions struct {
	*authtest.MemSessions
	down, touchDown atomic.Bool
}

func (s *bareSessions) Insert(ctx context.Context, sess session.Session) error {
	if s.down.Load() {
		return errPortDown
	}
	return s.MemSessions.Insert(ctx, sess)
}

func (s *bareSessions) ByHash(ctx context.Context, realm auth.Realm, hash string) (session.Session, error) {
	if s.down.Load() {
		return session.Session{}, errPortDown
	}
	return s.MemSessions.ByHash(ctx, realm, hash)
}

func (s *bareSessions) Touch(ctx context.Context, realm auth.Realm, hash string,
	seenAt, idleExpiresAt time.Time,
) error {
	if s.down.Load() || s.touchDown.Load() {
		return errPortDown
	}
	return s.MemSessions.Touch(ctx, realm, hash, seenAt, idleExpiresAt)
}

func (s *bareSessions) Delete(ctx context.Context, realm auth.Realm, hash string) error {
	if s.down.Load() {
		return errPortDown
	}
	return s.MemSessions.Delete(ctx, realm, hash)
}

func (s *bareSessions) DeleteOfSubject(ctx context.Context, realm auth.Realm, subjectID uuid.UUID) (int, error) {
	if s.down.Load() {
		return 0, errPortDown
	}
	return s.MemSessions.DeleteOfSubject(ctx, realm, subjectID)
}

func (s *bareSessions) DeleteExpired(ctx context.Context, realm auth.Realm, now time.Time) (int, error) {
	if s.down.Load() {
		return 0, errPortDown
	}
	return s.MemSessions.DeleteExpired(ctx, realm, now)
}

// bareAttempts — session.Attempts с тем же приёмом.
type bareAttempts struct {
	*authtest.MemAttempts
	down, recordDown atomic.Bool
}

func (a *bareAttempts) Count(ctx context.Context, realm auth.Realm, loginKey string, since time.Time) (int, error) {
	if a.down.Load() {
		return 0, errPortDown
	}
	return a.MemAttempts.Count(ctx, realm, loginKey, since)
}

func (a *bareAttempts) Record(ctx context.Context, attempt session.Attempt) error {
	if a.down.Load() || a.recordDown.Load() {
		return errPortDown
	}
	return a.MemAttempts.Record(ctx, attempt)
}

func (a *bareAttempts) Purge(ctx context.Context, realm auth.Realm, before time.Time) (int, error) {
	if a.down.Load() {
		return 0, errPortDown
	}
	return a.MemAttempts.Purge(ctx, realm, before)
}
