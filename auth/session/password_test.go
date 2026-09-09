package session_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

// СМЕНА ПАРОЛЯ ОТЗЫВАЕТ ВСЁ: все сессии субъекта, включая ту, из которой её
// заказали, и все живые токены сброса. Пароль меняют, когда подозревают чужой
// доступ, и сессия, пережившая смену, сводит эту смену на нет.
func TestService_ChangePassword_RevokesAll(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	first, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	second, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))
	require.Equal(t, 1, st.tokens.Live(testRealm, id.ID, token.PurposeReset))
	resetLink, ok := st.tokens.LastIssued()
	require.True(t, ok)

	err = st.svc.ChangePassword(t.Context(), session.ChangePasswordRequest{
		SubjectID: id.ID, Current: goodPassword, New: wrongPassword, IP: testIP, UserAgent: testUA,
	})

	require.NoError(t, err)
	for _, raw := range []string{first.Token, second.Token} {
		_, resolveErr := st.svc.Resolve(t.Context(), raw)
		require.ErrorIs(t, resolveErr, session.ErrNoSession, "сессия обязана умереть вместе со старым паролем")
	}
	assert.Zero(t, st.tokens.Live(testRealm, id.ID, token.PurposeReset),
		"ссылка сброса, выданная до смены, обязана умереть")
	// Та же ссылка после смены не открывает ничего: иначе перехвативший её
	// получает ровно то, от чего пароль и меняли.
	require.ErrorIs(t, st.svc.ConfirmReset(t.Context(), resetLink.RawToken, goodPassword),
		session.ErrTokenInvalid)

	// Новый пароль работает, старый нет.
	_, err = st.signIn(t, knownLogin, wrongPassword)
	require.NoError(t, err)
	_, err = st.signIn(t, knownLogin, goodPassword)
	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	assert.Equal(t, 1, st.notes.CountOf(session.NotifyPasswordChanged))
	assert.Equal(t, 1, st.journal.CountOf(session.EventPasswordChanged))
}

// ОТЗЫВ ИДЁТ ДО ЗАПИСИ НОВОГО ХЭША. На сбое записи владелец всего лишь
// выкинут из своих сессий и войдёт заново старым паролем; обратный порядок на
// том же сбое оставил бы чужую сессию живой рядом с новым паролем.
func TestService_ChangePassword_RevokesBeforeWritingHash(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	st.ids.SetPasswordErr = authtest.ErrInjected

	err = st.svc.ChangePassword(t.Context(), session.ChangePasswordRequest{
		SubjectID: id.ID, Current: goodPassword, New: wrongPassword,
	})

	require.ErrorIs(t, err, auth.ErrUnavailable)
	_, err = st.svc.Resolve(t.Context(), res.Token)
	require.ErrorIs(t, err, session.ErrNoSession, "сессии обязаны быть отозваны до неудачной записи")
	// Пароль не сменился, и это единственный исход, из которого владелец
	// выбирается сам: он входит старым паролем и повторяет смену.
	_, err = st.signIn(t, knownLogin, goodPassword)
	assert.NoError(t, err)
}

func TestService_ChangePassword_RefusesWrongCurrent(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)

	err = st.svc.ChangePassword(t.Context(), session.ChangePasswordRequest{
		SubjectID: id.ID, Current: wrongPassword, New: "sovsem-drugoy-parol-tut",
	})

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	_, err = st.svc.Resolve(t.Context(), res.Token)
	assert.NoError(t, err, "неудачная смена не должна ронять живые сессии")
}

func TestService_ChangePassword_RefusesUnknownSubject(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	err := st.svc.ChangePassword(t.Context(), session.ChangePasswordRequest{
		SubjectID: uuid.New(), Current: goodPassword, New: wrongPassword,
	})

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	require.NotErrorIs(t, err, auth.ErrIdentityNotFound, "отсутствие личности наружу не выдаётся")
}

func TestService_ChangePassword_ChecksPolicyWithLogin(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	st.strength.Set(wrongPassword, password.ScoreMin)

	err := st.svc.ChangePassword(t.Context(), session.ChangePasswordRequest{
		SubjectID: id.ID, Current: goodPassword, New: wrongPassword,
	})

	require.ErrorIs(t, err, password.ErrTooWeak)
	// Сессий не тронули: слабый пароль отбит до всякого отзыва.
	assert.Zero(t, st.sessions.CallCount("DeleteOfSubject"))
}

func TestService_Verification_RoundTrip(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	require.NoError(t, st.register(t, knownLogin, goodPassword))
	note, ok := st.tokens.LastIssued()
	require.True(t, ok)

	subject, err := st.svc.ConfirmVerification(t.Context(), note.RawToken)

	require.NoError(t, err)
	// Двойник ЧЕСТНО применяет эффект: без этого тест был бы зелёным при
	// неработающем подтверждении.
	rec, ok := st.ids.Get(subject)
	require.True(t, ok)
	assert.True(t, rec.Identity.Verified)
	assert.Equal(t, 1, st.journal.CountOf(session.EventVerified))

	// Одноразовость: тот же токен второй раз не работает.
	_, err = st.svc.ConfirmVerification(t.Context(), note.RawToken)
	require.ErrorIs(t, err, session.ErrTokenInvalid)
}

func TestService_Verification_ExpiredTokenIsInvalid(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	require.NoError(t, st.register(t, knownLogin, goodPassword))
	note, ok := st.tokens.LastIssued()
	require.True(t, ok)

	st.clock.Advance(st.cfg.VerifyTTL + 1)
	_, err := st.svc.ConfirmVerification(t.Context(), note.RawToken)

	require.ErrorIs(t, err, session.ErrTokenInvalid)
}

// Повторная отправка на неизвестный, негодный и уже подтверждённый адрес —
// тихий no-op: ответ, различающий эти случаи, та же проверялка существования.
// Без t.Parallel у родителя: утверждение после цикла смотрит на состояние,
// которое оставили подтесты.
func TestService_RequestVerification_IsQuietNoOp(t *testing.T) {
	st := newStand(t)
	st.seed(t, knownLogin) // уже подтверждён
	st.seed(t, "off@example.invalid", func(id *auth.Identity) { id.Disabled = true })

	for name, login := range map[string]string{
		"неизвестный адрес":   unknownLogin,
		"негодный адрес":      "alice\u200b@example.invalid",
		"уже подтверждён":     knownLogin,
		"отключённый субъект": "off@example.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, st.svc.RequestVerification(t.Context(), login))
		})
	}
	assert.Empty(t, st.tokens.Issued(), "ни одной ссылки уйти не должно")
}

func TestService_Reset_RoundTrip(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	old, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))
	note, ok := st.tokens.LastIssued()
	require.True(t, ok)
	assert.Equal(t, session.NotifyReset, note.Kind)

	require.NoError(t, st.svc.ConfirmReset(t.Context(), note.RawToken, wrongPassword))

	// Пароль сменился, старые сессии умерли — сброс это та же смена пароля.
	_, err = st.signIn(t, knownLogin, wrongPassword)
	require.NoError(t, err)
	_, err = st.svc.Resolve(t.Context(), old.Token)
	require.ErrorIs(t, err, session.ErrNoSession)
	assert.Zero(t, st.tokens.Live(testRealm, id.ID, token.PurposeReset))
	assert.Equal(t, 1, st.notes.CountOf(session.NotifyPasswordChanged))

	// Одноразовость под повтором.
	require.ErrorIs(t, st.svc.ConfirmReset(t.Context(), note.RawToken, goodPassword),
		session.ErrTokenInvalid)
}

// Повторный заказ сброса гасит прежнюю ссылку: две живые ссылки на один
// аккаунт — это два шанса перехвата вместо одного.
func TestService_RequestReset_RevokesPreviousLink(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))
	first, ok := st.tokens.LastIssued()
	require.True(t, ok)

	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))

	assert.Equal(t, 1, st.tokens.Live(testRealm, id.ID, token.PurposeReset))
	require.ErrorIs(t, st.svc.ConfirmReset(t.Context(), first.RawToken, wrongPassword),
		session.ErrTokenInvalid)
}

func TestService_RequestReset_IsQuietNoOpForUnknownLogin(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	require.NoError(t, st.svc.RequestReset(t.Context(), unknownLogin))

	assert.Empty(t, st.tokens.Issued())
}

func TestService_ConfirmReset_RejectsWeakPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))
	note, ok := st.tokens.LastIssued()
	require.True(t, ok)
	st.strength.Set(wrongPassword, password.ScoreMin)

	err := st.svc.ConfirmReset(t.Context(), note.RawToken, wrongPassword)

	require.ErrorIs(t, err, password.ErrTooWeak)
	// Токен не потрачен: иначе слабый пароль стоил бы человеку ссылки.
	assert.Equal(t, 1, st.tokens.Live(testRealm, st.mustID(t, knownLogin), token.PurposeReset))
}

func TestService_EmailChange_RoundTrip(t *testing.T) {
	t.Parallel()

	const newLogin = "alice.new@example.invalid"
	st := newStand(t)
	id := st.seed(t, knownLogin)

	require.NoError(t, st.svc.RequestEmailChange(t.Context(), session.EmailChangeRequest{
		SubjectID: id.ID, NewLogin: newLogin, Password: goodPassword,
	}))

	link, ok := st.tokens.LastIssued()
	require.True(t, ok)
	assert.Equal(t, session.NotifyEmailChange, link.Kind)
	assert.Equal(t, newLogin, link.Login, "ссылка уходит на НОВЫЙ адрес: он и доказывает владение")
	// Предупреждение уходит на ПРЕЖНИЙ адрес — единственный момент, когда его
	// ещё можно послать.
	assert.Equal(t, 1, st.notes.CountOf(session.NotifyLoginChangeRequested))
	notes := st.notes.Notifications()
	assert.Equal(t, knownLogin, notes[len(notes)-1].Login)

	subject, err := st.svc.ConfirmEmailChange(t.Context(), link.RawToken)

	require.NoError(t, err)
	assert.Equal(t, id.ID, subject)
	rec, ok := st.ids.Get(id.ID)
	require.True(t, ok)
	assert.Equal(t, newLogin, rec.Identity.Login)
	assert.Equal(t, 1, st.journal.CountOf(session.EventLoginChanged))
}

func TestService_RequestEmailChange_NeedsCurrentPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)

	err := st.svc.RequestEmailChange(t.Context(), session.EmailChangeRequest{
		SubjectID: id.ID, NewLogin: "alice.new@example.invalid", Password: wrongPassword,
	})

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	assert.Empty(t, st.tokens.Issued())
}

// Занятость нового адреса ДО подтверждения не проверяется: проверка сделала бы
// ручку проверялкой существования для любого, кто завёл аккаунт. Правда
// всплывает на подтверждении — там владение адресом уже доказано.
func TestService_EmailChange_TakenLoginSurfacesOnlyOnConfirm(t *testing.T) {
	t.Parallel()

	const busyLogin = "bob@example.invalid"
	st := newStand(t)
	id := st.seed(t, knownLogin)
	st.seed(t, busyLogin)

	require.NoError(t, st.svc.RequestEmailChange(t.Context(), session.EmailChangeRequest{
		SubjectID: id.ID, NewLogin: busyLogin, Password: goodPassword,
	}), "занятость на заказе не проверяется")
	link, ok := st.tokens.LastIssued()
	require.True(t, ok)

	_, err := st.svc.ConfirmEmailChange(t.Context(), link.RawToken)

	require.ErrorIs(t, err, auth.ErrLoginTaken)
	rec, ok := st.ids.Get(id.ID)
	require.True(t, ok)
	assert.Equal(t, knownLogin, rec.Identity.Login, "логин обязан остаться прежним")
	// Токен не погашен: эффект не применился, значит и гасить нечего —
	// в адаптере это откат транзакции.
	assert.Equal(t, 1, st.tokens.Live(testRealm, id.ID, token.PurposeEmailChange))
}

func TestService_Confirm_RejectsEmptyAndUnknownToken(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	for name, raw := range map[string]string{
		"пусто": "",
		"мусор": "not-a-token",
		"чужой": "0000000000000000000000000000000000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := st.svc.ConfirmVerification(t.Context(), raw)
			require.ErrorIs(t, err, session.ErrTokenInvalid)
			require.ErrorIs(t, st.svc.ConfirmReset(t.Context(), raw, goodPassword), session.ErrTokenInvalid)
			_, err = st.svc.ConfirmEmailChange(t.Context(), raw)
			require.ErrorIs(t, err, session.ErrTokenInvalid)
		})
	}
}

// Сырой токен не попадает ни в одну запись аудита: журнал живёт дольше всего.
func TestService_Audit_CarriesNoRawToken(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	res, err := st.signIn(t, knownLogin, goodPassword)
	require.NoError(t, err)
	require.NoError(t, st.svc.RequestReset(t.Context(), knownLogin))
	link, ok := st.tokens.LastIssued()
	require.True(t, ok)

	for _, ev := range st.journal.Events() {
		assert.NotEqual(t, res.Token, ev.SessionHash, "в событии обязан быть хэш, а не токен")
		assert.NotContains(t, ev.SessionHash, link.RawToken)
	}
}

func (s *stand) mustID(t *testing.T, login string) uuid.UUID {
	t.Helper()
	id, err := s.ids.ByLogin(t.Context(), login)
	require.NoError(t, err)
	return id.ID
}

// Повторная отправка ссылки подтверждения РАБОТАЕТ — тихий no-op бывает только
// у неизвестного, негодного и уже подтверждённого адреса.
func TestService_RequestVerification_SendsLinkToUnverified(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin, func(id *auth.Identity) { id.Verified = false })

	require.NoError(t, st.svc.RequestVerification(t.Context(), knownLogin))

	note, ok := st.tokens.LastIssued()
	require.True(t, ok, "ссылка подтверждения обязана уйти")
	assert.Equal(t, session.NotifyVerify, note.Kind)
	assert.Equal(t, knownLogin, note.Login)
	assert.Equal(t, 1, st.tokens.Live(testRealm, id.ID, token.PurposeVerify))
	assert.Equal(t, 1, st.journal.CountOf(session.EventTokenIssued))
}

// Сбой выдачи токена наружу не проглатывается: «ссылка отправлена» при
// незаписанной строке — это письмо в никуда, которого человек будет ждать.
func TestService_RequestReset_FailsClosedWhenIssueFails(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	st.tokens.Err = authtest.ErrInjected

	err := st.svc.RequestReset(t.Context(), knownLogin)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.Zero(t, st.journal.CountOf(session.EventTokenIssued),
		"событие «токен выдан» без выданного токена — ложь в журнале")
}
