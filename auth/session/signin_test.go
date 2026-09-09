package session_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

func TestService_SignIn_HappyPath(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)

	res, err := st.signIn(t, knownLogin, goodPassword)

	require.NoError(t, err)
	assert.Equal(t, id.ID, res.Principal.SubjectID)
	assert.Equal(t, testRealm, res.Principal.Realm)
	assert.Len(t, res.Token, token.RawLen, "сырой токен — 256 бит в base64 без выравнивания")
	// В ПРИНЦИПАЛЕ ХЭШ, А НЕ ТОКЕН: принципал уезжает в контекст запроса и в
	// аудит потребителя, и сырой токен там означал бы токен в логе.
	assert.NotEqual(t, res.Token, res.Principal.SessionHash)
	assert.Equal(t, token.Hash(res.Token, testSecret()), res.Principal.SessionHash)
	assert.Equal(t, 1, st.sessions.Len())
	assert.Zero(t, st.attempts.Len(), "удачный вход попыткой не считается")
	assert.Equal(t, 1, st.journal.CountOf(session.EventSignedIn))
}

// ПАРИТЕТ — ГЛАВНЫЙ ИНВАРИАНТ ВХОДА. Существующий и несуществующий логин
// обязаны дать один ответ И одни и те же обращения к портам: разница в
// вызовах видна секундомером, и она дороже любой разницы в тексте.
//
// Тест НЕ параллельный: ветка про ErrBusy занимает потолок хеширований, общий
// на процесс, и параллельный сосед получил бы ErrBusy вместо своего сценария.
func TestService_SignIn_ParityForUnknownLogin(t *testing.T) {
	known := newStand(t)
	known.seed(t, knownLogin)
	known.resetCalls()
	unknown := newStand(t)

	knownErr := signInErr(t, known, knownLogin)
	unknownErr := signInErr(t, unknown, unknownLogin)

	require.ErrorIs(t, knownErr, session.ErrInvalidCredentials)
	require.ErrorIs(t, unknownErr, session.ErrInvalidCredentials)
	assert.Equal(t, knownErr.Error(), unknownErr.Error(), "тексты ответов обязаны совпасть")
	assert.Equal(t, known.portCalls(), unknown.portCalls(),
		"обращения к портам разошлись: по ним видно, есть ли такой адрес")
	// Попытка пишется в обеих ветках: счётчик, ведущийся только для
	// существующих логинов, сам отвечает на вопрос «есть ли такой адрес».
	assert.Equal(t, 1, known.attempts.Len())
	assert.Equal(t, 1, unknown.attempts.Len())

	t.Run("при занятом потолке хеширований", func(t *testing.T) {
		busyKnown := newStand(t, quickGiveUp)
		busyKnown.seed(t, knownLogin)
		busyKnown.resetCalls()
		busyUnknown := newStand(t, quickGiveUp)

		var knownBusy, unknownBusy error
		withFullGate(t, func() {
			knownBusy = signInErr(t, busyKnown, knownLogin)
			unknownBusy = signInErr(t, busyUnknown, unknownLogin)
		})

		require.ErrorIs(t, knownBusy, session.ErrTooManyAttempts)
		require.ErrorIs(t, unknownBusy, session.ErrTooManyAttempts)
		assert.Equal(t, knownBusy.Error(), unknownBusy.Error())
		assert.Equal(t, busyKnown.portCalls(), busyUnknown.portCalls(),
			"на перегрузке обращения к портам разошлись — потолок стал каналом перебора")
		// Перегрузка не считается попыткой: иначе всплеск запирает законных
		// владельцев счётчиком, которого они не заслужили.
		assert.Zero(t, busyKnown.attempts.Len())
		assert.Zero(t, busyUnknown.attempts.Len())
	})
}

// СЧЁТЧИК ВЕДЁТСЯ ПО ЛОГИНУ, КОТОРОГО НЕТ. Иначе перебор адресов не стоит
// ничего: неизвестный логин не оставляет следа, и его можно проверять вечно.
func TestService_Lockout_CountsUnknownLogins(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) { c.LockoutAttempts = 3 }))

	for range 3 {
		require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)
	}
	require.Equal(t, 3, st.attempts.Len(), "попытки по несуществующему логину обязаны считаться")

	st.resetCalls()
	err := signInErr(t, st, unknownLogin)

	require.ErrorIs(t, err, session.ErrTooManyAttempts)
	assert.Zero(t, st.ids.CallCount("ByLogin"),
		"запертый вход не должен ходить за личностью: в этом и смысл счётчика")
	// Исчерпанный счётчик не пополняется: иначе любой желающий запирает чужой
	// адрес навсегда, просто продолжая стучаться.
	assert.Equal(t, 3, st.attempts.Len())
	assert.Equal(t, 1, st.journal.CountOf(session.EventLockedOut))
}

// Разные написания одного адреса дают ОДИН ключ счётчика: иначе блокировка
// обходится клавишей Shift.
func TestService_Lockout_KeyIsNormalized(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) { c.LockoutAttempts = 2 }))

	require.ErrorIs(t, signInErr(t, st, "Alice@Example.INVALID"), session.ErrInvalidCredentials)
	require.ErrorIs(t, signInErr(t, st, knownLogin), session.ErrInvalidCredentials)

	require.ErrorIs(t, signInErr(t, st, "ALICE@example.invalid"), session.ErrTooManyAttempts)
}

// Окно съезжает: попытки за его пределами в счёт не идут.
func TestService_Lockout_WindowSlides(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) {
		c.LockoutAttempts = 2
		c.LockoutWindow = 10 * time.Minute
	}))
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials)
	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrTooManyAttempts)

	st.clock.Advance(11 * time.Minute)

	require.ErrorIs(t, signInErr(t, st, unknownLogin), session.ErrInvalidCredentials,
		"после окна счётчик обязан отпустить")
}

// Неподтверждённый адрес — отказ, и он достижим ТОЛЬКО после верного пароля:
// тот, кто его увидел, и так знает и адрес, и пароль.
func TestService_SignIn_UnverifiedIsRefusedByDefault(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin, func(id *auth.Identity) { id.Verified = false })

	_, err := st.signIn(t, knownLogin, goodPassword)

	require.ErrorIs(t, err, session.ErrNotVerified)
	assert.Zero(t, st.sessions.Len())
	// Верный пароль попыткой не считается: иначе неподтверждённый владелец
	// запирал бы себе адрес, пытаясь войти.
	assert.Zero(t, st.attempts.Len())
}

func TestService_SignIn_UnverifiedPassesWhenAllowed(t *testing.T) {
	t.Parallel()

	st := newStand(t, withConfig(func(c *session.Config) { c.AllowUnverifiedSignIn = true }))
	st.seed(t, knownLogin, func(id *auth.Identity) { id.Verified = false })

	_, err := st.signIn(t, knownLogin, goodPassword)

	require.NoError(t, err)
	assert.Equal(t, 1, st.sessions.Len())
}

// Отключённый субъект отвечает как неверный пароль: отдельный ответ на него
// выдал бы существование адреса.
func TestService_SignIn_DisabledLooksLikeWrongPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin, func(id *auth.Identity) { id.Disabled = true })

	_, err := st.signIn(t, knownLogin, goodPassword)

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	assert.Equal(t, 1, st.attempts.Len())
	assert.Zero(t, st.sessions.Len())
}

// Битый хэш в колонке — тоже «неверные данные», а не отдельный ответ: битую
// строку может иметь только СУЩЕСТВУЮЩИЙ логин.
func TestService_SignIn_MalformedHashLooksLikeWrongPassword(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin, func(id *auth.Identity) { id.PasswordHash = "$argon2id$broken" })

	_, err := st.signIn(t, knownLogin, goodPassword)

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	require.NotErrorIs(t, err, password.ErrHashInvalid, "дефект данных наружу не выдаётся")
	assert.Equal(t, 1, st.attempts.Len())
}

// Непрочитанный счётчик — отказ, а не пропуск: вход, работающий при сломанной
// защите от перебора, — это вход без защиты от перебора.
func TestService_SignIn_FailsClosedWhenAttemptsAreUnreadable(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	st.attempts.Err = authtest.ErrInjected

	_, err := st.signIn(t, knownLogin, goodPassword)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.Zero(t, st.sessions.Len())
}

// Незаписанная попытка — тоже отказ: перебор без счёта хуже отказа. Счёт при
// этом проходит, роняется ровно запись.
func TestService_SignIn_FailsClosedWhenAttemptIsNotRecorded(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	st.attempts.RecordErr = authtest.ErrInjected

	_, err := st.signIn(t, knownLogin, wrongPassword)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	require.NotErrorIs(t, err, session.ErrInvalidCredentials,
		"незаписанная попытка не должна выглядеть как обычный неверный пароль")
}

// Хэш ниже нынешних параметров пересчитывается на удачном входе:
// перехеширование возможно только там, где видны и пароль, и старый хэш.
func TestService_SignIn_RehashesWeakHash(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seedWeak(t, knownLogin)

	res, err := st.signIn(t, knownLogin, goodPassword)

	require.NoError(t, err)
	assert.True(t, res.Rehashed, "хэш ниже нынешних параметров обязан пересчитаться")
	rec, ok := st.ids.Get(id.ID)
	require.True(t, ok)
	assert.False(t, st.hasher.NeedsRehash(rec.Identity.PasswordHash))
}

// Сбой пересчёта не отменяет входа: пароль уже проверен, и запирать человека
// снаружи из-за недоступной таблицы значило бы менять вход на обслуживание.
func TestService_SignIn_RehashFailureDoesNotDenyEntry(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seedWeak(t, knownLogin)
	st.ids.SetPasswordErr = authtest.ErrInjected

	res, err := st.signIn(t, knownLogin, goodPassword)

	require.NoError(t, err, "вход обязан состояться и без пересчёта")
	assert.False(t, res.Rehashed)
	rec, ok := st.ids.Get(id.ID)
	require.True(t, ok)
	assert.True(t, st.hasher.NeedsRehash(rec.Identity.PasswordHash), "хэш остался старым")
}

// Ненормализуемый логин — отказ без похода в порты: ключа счётчика для него
// нет, а ничьим адресом он быть не может.
func TestService_SignIn_RejectsUnnormalizableLogin(t *testing.T) {
	t.Parallel()

	st := newStand(t)

	// Символ нулевой ширины: NFKC его не убирает, а loginid.Normalize
	// отвергает — печатается такой логин неотличимо от чужого.
	_, err := st.signIn(t, "alice\u200b@example.invalid", goodPassword)

	require.ErrorIs(t, err, session.ErrInvalidCredentials)
	assert.Zero(t, st.attempts.CallCount("Count"))
	assert.Zero(t, st.ids.CallCount("ByLogin"))
}

// User-Agent обрезается до потолка схемы: без обрезки килобайтный заголовок
// ронял бы вставку сессии на CHECK, то есть закрывал бы вход одним запросом.
func TestService_SignIn_ClampsUserAgent(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	id := st.seed(t, knownLogin)
	long := strings.Repeat("x", session.MaxUserAgentLen*4)

	res, err := st.svc.SignIn(t.Context(), session.SignInRequest{
		Login: knownLogin, Password: goodPassword, IP: testIP, UserAgent: long,
	})

	require.NoError(t, err)
	assert.Len(t, res.Session.UserAgent, session.MaxUserAgentLen)
	assert.Equal(t, id.ID, res.Session.SubjectID)
}

// signInErr — вход заведомо неверным паролем; возвращает только ошибку.
func signInErr(t *testing.T, st *stand, login string) error {
	t.Helper()
	_, err := st.signIn(t, login, wrongPassword)
	require.Error(t, err)
	return err
}

// quickGiveUp — стенд, который отваливается по ErrBusy почти сразу: ждать
// свободный слот в тесте про перегрузку незачем.
func quickGiveUp(st *stand) {
	cfg := testHasherConfig()
	cfg.MaxWait = time.Millisecond
	st.hasher = password.NewHasher(cfg)
}

// withFullGate занимает ВЕСЬ потолок хеширований процесса на время fn.
//
// ЗАПОЛНЕНИЕ, А НЕ ЗАМЕР. Каждая горутина делает РОВНО ОДИН дорогой хэш и
// держит слот до конца, поэтому щели, в которую мог бы проскочить вход, не
// существует вовсе — в отличие от цикла, где слот на мгновение освобождается
// между итерациями. Ждём, пока занятость станет равной потолку, и только
// потом зовём fn.
func withFullGate(t *testing.T, fn func()) {
	t.Helper()
	filler := password.NewHasher(password.DefaultHasherConfig())
	var wg sync.WaitGroup
	wg.Add(password.MinSlots)
	for range password.MinSlots {
		go func() {
			defer wg.Done()
			_, _ = filler.Hash(context.WithoutCancel(t.Context()), goodPassword)
		}()
	}
	require.Eventually(t, func() bool { return filler.InFlight() == password.MinSlots },
		30*time.Second, time.Millisecond, "потолок хеширований так и не заполнился")

	fn()
	wg.Wait()
}
