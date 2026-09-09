package session_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
)

func TestService_Resolve_ReturnsPrincipal(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	p, err := st.svc.Resolve(t.Context(), res.Token)

	require.NoError(t, err)
	assert.Equal(t, id.ID, p.SubjectID)
	assert.Equal(t, testRealm, p.Realm)
	assert.Equal(t, res.Principal.SessionHash, p.SessionHash)
	assert.NotEqual(t, res.Token, p.SessionHash, "в принципал уезжает хэш, а не токен")
}

// СКОЛЬЗЯЩЕЕ ПРОДЛЕНИЕ НЕ ЧАЩЕ RenewEvery: без порога каждый запрос страницы
// превращается в запись в таблицу сессий.
func TestService_Resolve_RenewsNoMoreOftenThanRenewEvery(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) { c.RenewEvery = 5 * time.Minute }))
	st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	st.resetCalls()

	st.clock.Advance(time.Minute)
	_, err = st.svc.Resolve(t.Context(), res.Token)
	require.NoError(t, err)
	assert.Zero(t, st.sessions.CallCount("Touch"), "до порога продлевать нечего")

	// РОВНО порог: продление обязано сработать на равенстве, иначе сессия
	// того, кто ходит ровно раз в RenewEvery, не продлевается никогда.
	st.clock.Advance(4 * time.Minute)
	_, err = st.svc.Resolve(t.Context(), res.Token)
	require.NoError(t, err)
	assert.Equal(t, 1, st.sessions.CallCount("Touch"))

	sess, err := st.sessions.ByHash(t.Context(), testRealm, res.Principal.SessionHash)
	require.NoError(t, err)
	assert.Equal(t, st.clock.Now(), sess.LastSeenAt)
	// Абсолютный срок не двигается никогда: иначе сессия того, кто ходит чаще
	// RenewEvery, живёт вечно.
	assert.Equal(t, res.Session.ExpiresAt, sess.ExpiresAt)
}

// Скользящий срок зажат абсолютным: CHECK схемы требует idle <= expires, и
// строка с продлением за абсолютный срок просто не записалась бы.
func TestService_Resolve_IdleDeadlineIsClampedByAbsolute(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) {
		c.SessionTTL = 30 * time.Minute
		c.IdleTTL = 25 * time.Minute
		c.RenewEvery = time.Minute
	}))
	st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	st.clock.Advance(20 * time.Minute)
	_, err = st.svc.Resolve(t.Context(), res.Token)
	require.NoError(t, err)

	sess, err := st.sessions.ByHash(t.Context(), testRealm, res.Principal.SessionHash)
	require.NoError(t, err)
	assert.Equal(t, sess.ExpiresAt, sess.IdleExpiresAt,
		"скользящий срок обязан упереться в абсолютный, а не перерасти его")
}

func TestService_Resolve_RefusesExpired(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mod     func(*session.Config)
		advance time.Duration
	}{
		"по абсолютному сроку": {
			mod:     func(c *session.Config) { c.SessionTTL = time.Hour; c.IdleTTL = time.Hour },
			advance: 61 * time.Minute,
		},
		"по простою": {
			mod:     func(c *session.Config) { c.SessionTTL = 72 * time.Hour; c.IdleTTL = 10 * time.Minute },
			advance: 11 * time.Minute,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := newStand(t, withConfig(func(c *session.Config) {
				tc.mod(c)
				c.RenewEvery = time.Minute
			}))
			st.seed(t, knownLogin)
			res, err := st.signIn(t, knownLogin, goodPassword)
			require.NoError(t, err)

			st.clock.Advance(tc.advance)
			_, err = st.svc.Resolve(t.Context(), res.Token)

			require.ErrorIs(t, err, session.ErrNoSession)
			// Негодная строка гасится сразу: держать её значит копить мусор,
			// который уборка разберёт когда-нибудь потом.
			assert.Zero(t, st.sessions.Len())
		})
	}
}

// ОТКЛЮЧЁННЫЙ СУБЪЕКТ ОТВЕРГАЕТСЯ СРАЗУ, не дожидаясь истечения сессии, —
// иначе уволенный сотрудник работает до конца SessionTTL.
func TestService_Resolve_RefusesDisabledSubjectImmediately(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	st.disable(t, id.ID)

	_, err = st.svc.Resolve(t.Context(), res.Token)

	require.ErrorIs(t, err, session.ErrNoSession)
	assert.Zero(t, st.sessions.Len(), "сессия отключённого обязана исчезнуть")
}

func TestService_Resolve_RefusesUnknownAndEmptyToken(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	for name, raw := range map[string]string{
		"пустой токен":   "",
		"чужой токен":    "0000000000000000000000000000000000000000000",
		"мусор в строке": "not-a-token",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := st.svc.Resolve(t.Context(), raw)
			require.ErrorIs(t, err, session.ErrNoSession)
		})
	}
}

// Сбой продления — отказ: хранилище, в которое нельзя писать, отдаёт сессии,
// которые молча умрут по IdleTTL, и потребитель узнает об этом массовым
// разлогином, а не алертом.
func TestService_Resolve_FailsClosedWhenTouchFails(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) { c.RenewEvery = time.Minute }))
	st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	st.clock.Advance(2 * time.Minute)
	st.sessions.TouchErr = authtest.ErrInjected
	_, err = st.svc.Resolve(t.Context(), res.Token)

	require.ErrorIs(t, err, auth.ErrUnavailable)
}

func TestService_SignOut_IsIdempotent(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	require.NoError(t, st.svc.SignOut(t.Context(), res.Token))
	require.NoError(t, st.svc.SignOut(t.Context(), res.Token), "выход обязан быть идемпотентным")
	require.NoError(t, st.svc.SignOut(t.Context(), ""))

	assert.Zero(t, st.sessions.Len())
	_, err = st.svc.Resolve(t.Context(), res.Token)
	require.ErrorIs(t, err, session.ErrNoSession)
}

func TestService_SignOutAll_KillsEveryDevice(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	first, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	second, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	stranger := st.seed(t, "bob@example.invalid")
	strangerSession, err := st.signIn(t, "bob@example.invalid", goodPassword)
	require.NoError(t, err)

	n, err := st.svc.SignOutAll(t.Context(), id.ID)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	for _, raw := range []string{first.Token, second.Token} {
		_, err = st.svc.Resolve(t.Context(), raw)
		require.ErrorIs(t, err, session.ErrNoSession)
	}
	p, err := st.svc.Resolve(t.Context(), strangerSession.Token)
	require.NoError(t, err, "чужие сессии обязаны уцелеть")
	assert.Equal(t, stranger.ID, p.SubjectID)
}

// Sweep убирает все три таблицы и возвращает сумму: подпись совпадает со
// scheduler.Job.Run — в этом весь контракт с планировщиком.
func TestService_Sweep_ClearsSessionsAttemptsAndTokens(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) {
		c.SessionTTL = time.Hour
		c.IdleTTL = time.Hour
		c.RenewEvery = time.Minute
		c.LockoutWindow = 15 * time.Minute
	}))
	st.seed(t, knownLogin)
	_, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))

	st.clock.Advance(48 * time.Hour)
	n, err := st.svc.Sweep(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 3, n, "сессия, попытка и токен обязаны уйти одним проходом")
	assert.Zero(t, st.sessions.Len())
	assert.Zero(t, st.attempts.Len())
}

func TestService_Sweep_FailsClosedOnStoreError(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.sessions.Err = authtest.ErrInjected

	_, err := st.svc.Sweep(t.Context())

	require.ErrorIs(t, err, auth.ErrUnavailable)
}

// Сессии и попытки чужого реалма второй сервис не видит: реалм входит в ключ
// строки и в каждый WHERE.
func TestService_Realms_DoNotSeeEachOther(t *testing.T) {
	t.Parallel()

	shop := newStand(t)
	shop.seed(t, knownLogin)
	res, err := shop.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	staff := session.New(shop.deps(), session.DefaultConfig("staff", testSecret()))
	staff.SetClock(shop.clock.Now)

	_, err = staff.Resolve(t.Context(), res.Token)

	require.ErrorIs(t, err, session.ErrNoSession)
	n, err := staff.SignOutAll(t.Context(), uuid.Nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, shop.sessions.Len(), "чужой реалм не должен трогать наши строки")
}

// ОКНО ОТСЧИТЫВАЕТСЯ НАЗАД, А НЕ ВПЕРЁД. Уборка, зовущая Purge с now плюс
// окном, вычищает и свежие попытки — то есть обнуляет защиту от перебора ровно
// тем механизмом, который её обслуживает.
func TestService_Sweep_PurgesAttemptsOlderThanTheWindow(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) {
		c.LockoutWindow = 15 * time.Minute
		c.LockoutAttempts = 10
	}))
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)
	st.clock.Advance(20 * time.Minute)
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)

	n, err := st.svc.Sweep(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, n, "убрана обязана быть ровно одна попытка — та, что старше окна")
	assert.Equal(t, 1, st.attempts.Len(), "свежая попытка обязана уцелеть")
}

// Sweep возвращает то, что успел убрать ДО сбоя: цифра «ноль» на прогоне, где
// две таблицы уже вычищены, отправила бы дежурного искать несуществующую
// проблему с уборкой сессий.
func TestService_Sweep_ReportsWhatItManagedToClear(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) {
		c.SessionTTL = time.Hour
		c.IdleTTL = time.Hour
		c.RenewEvery = time.Minute
		c.LockoutWindow = 15 * time.Minute
	}))
	st.seed(t, knownLogin)
	_, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)

	st.clock.Advance(48 * time.Hour)
	st.tokens.Err = authtest.ErrInjected
	n, err := st.svc.Sweep(t.Context())

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.Equal(t, 2, n, "сессия и попытка убраны — их и обязан назвать ответ")
}
