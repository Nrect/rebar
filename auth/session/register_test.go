package session_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

func TestService_Register_CreatesAndSendsVerification(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	require.NoError(t, st.register(t, knownLogin, goodPassword))

	assert.Equal(t, 1, st.ids.Len())
	note, ok := st.tokens.LastIssued()
	require.True(t, ok, "ссылка подтверждения обязана уйти")
	assert.Equal(t, session.NotifyVerify, note.Kind)
	assert.Equal(t, knownLogin, note.Login)
	assert.Len(t, note.RawToken, token.RawLen)
	assert.Equal(t, 1, st.journal.CountOf(session.EventRegistered))
}

// СЕМАНТИКА «ПРИНЯТО», А НЕ «АДРЕС ЗАНЯТ». Ответ на занятый адрес тот же
// самый, а правду узнаёт ВЛАДЕЛЕЦ адреса — письмом. Иначе форма регистрации
// становится проверялкой существования, работающей без пароля и без счётчика.
func TestService_Register_TakenLoginLooksAccepted(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	require.NoError(t, st.register(t, knownLogin, goodPassword))
	before := st.ids.Len()

	err := st.register(t, knownLogin, goodPassword)

	require.NoError(t, err, "занятый адрес обязан выглядеть как принятая заявка")
	require.NotErrorIs(t, err, auth.ErrLoginTaken)
	assert.Equal(t, before, st.ids.Len(), "второй личности появиться не должно")
	assert.Equal(t, 1, st.notes.CountOf(session.NotifyLoginTaken),
		"владелец адреса обязан узнать о попытке")
	// Второй ссылки подтверждения нет: письмо со ссылкой ушло бы тому, кто
	// адресом не владеет, вместе с рабочим токеном.
	assert.Len(t, st.tokens.Issued(), 1)
}

// Обе ветки регистрации стоят одного argon2id: хэш считается ДО Create,
// поэтому занятый адрес не отличить от свободного по времени ответа. Проверка
// структурная, а не по секундомеру: хэш обязан быть посчитан и в той ветке,
// где личность не создаётся.
func TestService_Register_HashesBeforeCreate(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	require.NoError(t, st.register(t, knownLogin, goodPassword))
	st.resetCalls()

	require.NoError(t, st.register(t, knownLogin, goodPassword))

	assert.Equal(t, 1, st.ids.CallCount("Create"),
		"Create обязан быть позван и на занятом адресе: иначе ветки различимы")
}

func TestService_Register_RejectsWeakPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.strength.Set(wrongPassword, password.ScoreMin)

	err := st.register(t, knownLogin, wrongPassword)

	require.ErrorIs(t, err, password.ErrTooWeak)
	assert.Zero(t, st.ids.Len())
	assert.Zero(t, st.ids.CallCount("Create"), "слабый пароль не должен доходить до хранилища")
}

// Короткий пароль отбивается до всего остального: длина стоит наносекунды и
// отсекает гигантский ввод до квадратичной оценки силы.
func TestService_Register_RejectsShortPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	err := st.register(t, knownLogin, "korotko")

	require.ErrorIs(t, err, password.ErrTooShort)
	assert.Zero(t, st.ids.CallCount("Create"))
}

func TestService_Register_RejectsBadLogin(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	for name, login := range map[string]string{
		"пусто":            "",
		"невидимый символ": "alice\u200b@example.invalid",
		"длиннее потолка":  strings.Repeat("a", loginid.MaxLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := st.register(t, login, goodPassword)
			require.ErrorIs(t, err, loginid.ErrInvalid)
		})
	}
}

// Логин нормализуется РОВНО ОДИН РАЗ, на входе в сервис: в порт он приходит
// уже нормализованным, а второй точки нормализации нет.
func TestService_Register_NormalizesLoginOnce(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	require.NoError(t, st.register(t, "  Alice@Example.INVALID  ", goodPassword))

	stored, err := st.ids.ByLogin(t.Context(), knownLogin)
	require.NoError(t, err, "в хранилище обязан лежать нормализованный логин")
	assert.Equal(t, knownLogin, stored.Login)
}

// Сбой хранилища личностей — 503, а не «принято»: тихая потеря регистрации
// выглядит для человека как отправленное письмо, которого не будет.
func TestService_Register_FailsClosedOnStoreError(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.ids.Err = authtest.ErrInjected

	err := st.register(t, knownLogin, goodPassword)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.Empty(t, st.tokens.Issued())
}

func (s *stand) register(t *testing.T, login, pw string) error {
	t.Helper()
	return s.svc.Register(t.Context(), session.RegisterRequest{
		Login: login, Password: pw, IP: testIP, UserAgent: testUA,
	})
}
