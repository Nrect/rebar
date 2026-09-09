package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

// НЕГОДНЫЙ Config РОНЯЕТ ПРОЦЕСС НА СТАРТЕ, а не на первом входе. Тест
// табличный по каждому обязательному полю и сверяет не только факт паники, но
// и то, что она НАЗЫВАЕТ поле: сообщение читает тот, кто собирает сервис.
func TestNew_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate func(*session.Config)
		field  string
	}{
		"нулевой":                 {func(c *session.Config) { *c = session.Config{} }, "Config.Realm"},
		"реалм не той формы":      {func(c *session.Config) { c.Realm = "Shop" }, "Config.Realm"},
		"реалм пуст":              {func(c *session.Config) { c.Realm = "" }, "Config.Realm"},
		"секрет не настроен":      {func(c *session.Config) { c.Secret = token.Secret{} }, "Config.Secret"},
		"скользящий срок нулевой": {func(c *session.Config) { c.IdleTTL = 0 }, "Config.IdleTTL"},
		"скользящий больше абсолютного": {
			func(c *session.Config) { c.IdleTTL = c.SessionTTL + time.Minute }, "Config.IdleTTL",
		},
		"абсолютный срок нулевой": {func(c *session.Config) { c.SessionTTL = 0 }, "Config.IdleTTL"},
		"продление нулевое":       {func(c *session.Config) { c.RenewEvery = 0 }, "Config.RenewEvery"},
		"продление не меньше скользящего": {
			func(c *session.Config) { c.RenewEvery = c.IdleTTL }, "Config.RenewEvery",
		},
		"попыток ноль":          {func(c *session.Config) { c.LockoutAttempts = 0 }, "Config.LockoutAttempts"},
		"попыток меньше нуля":   {func(c *session.Config) { c.LockoutAttempts = -1 }, "Config.LockoutAttempts"},
		"окно нулевое":          {func(c *session.Config) { c.LockoutWindow = 0 }, "Config.LockoutWindow"},
		"подтверждение нулевое": {func(c *session.Config) { c.VerifyTTL = 0 }, "Config.VerifyTTL"},
		"подтверждение за потолком": {
			func(c *session.Config) { c.VerifyTTL = session.MaxVerifyTTL + time.Minute }, "Config.VerifyTTL",
		},
		"сброс нулевой": {func(c *session.Config) { c.ResetTTL = 0 }, "Config.ResetTTL"},
		"сброс за потолком": {
			func(c *session.Config) { c.ResetTTL = session.MaxResetTTL + time.Minute }, "Config.ResetTTL",
		},
		"смена логина нулевая": {func(c *session.Config) { c.EmailChangeTTL = 0 }, "Config.EmailChangeTTL"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := newStand(t)
			cfg := session.DefaultConfig(testRealm, testSecret())
			tc.mutate(&cfg)

			defer func() {
				got := recover()
				require.NotNilf(t, got, "%s: конструктор обязан упасть", name)
				text, ok := got.(string)
				require.Truef(t, ok, "%s: паника обязана быть текстом", name)
				// Проверяется НАЧАЛО сообщения, а не вхождение: тексты
				// ссылаются друг на друга («RenewEvery … less than IdleTTL»),
				// и проверка вхождением принимала бы сообщение о соседнем поле
				// за сообщение о нужном.
				assert.Truef(t, strings.HasPrefix(text, "session.New: "+tc.field+" must"),
					"%s: паника обязана начинаться с %q, получено %q", name, tc.field, text)
			}()
			session.New(st.deps(), cfg)
		})
	}
}

// ГРАНИЦЫ ВКЛЮЧИТЕЛЬНЫЕ, и это не мелочь: инвариант ADR-0003 звучит как
// «0 < IdleTTL <= SessionTTL» и «ResetTTL <= 1h», а конструктор, отвергающий
// само значение потолка, заставляет потребителя подгонять конфиг на глазок.
func TestNew_AcceptsValuesExactlyAtTheBoundary(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*session.Config){
		"скользящий срок равен абсолютному": func(c *session.Config) { c.IdleTTL = c.SessionTTL },
		"подтверждение ровно на потолке":    func(c *session.Config) { c.VerifyTTL = session.MaxVerifyTTL },
		"сброс ровно на потолке":            func(c *session.Config) { c.ResetTTL = session.MaxResetTTL },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := newStand(t)
			cfg := session.DefaultConfig(testRealm, testSecret())
			mutate(&cfg)

			assert.NotPanics(t, func() { session.New(st.deps(), cfg) })
		})
	}
}

// Потолки сроков — против абсурда в конфиге потребителя: ссылка сброса,
// живущая неделю, лежит в почтовом ящике ровно столько же.
func TestConfig_CeilingsMatchADR(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Hour, session.MaxResetTTL)
	assert.Equal(t, 72*time.Hour, session.MaxVerifyTTL)
}

// Нулевое значение AllowUnverifiedSignIn — строгая регистрация: fail-closed
// достаётся тому, кто про поле не знал.
func TestDefaultConfig_IsStrictAndValid(t *testing.T) {
	t.Parallel()

	cfg := session.DefaultConfig(testRealm, testSecret())

	assert.False(t, cfg.AllowUnverifiedSignIn)
	assert.LessOrEqual(t, cfg.ResetTTL, session.MaxResetTTL)
	assert.LessOrEqual(t, cfg.VerifyTTL, session.MaxVerifyTTL)

	st := newStand(t)
	assert.NotPanics(t, func() { session.New(st.deps(), cfg) })
}

// Секрет реалма не печатается ни одним глаголом: снимок конфига в отладочной
// ручке иначе выкладывает ключ HMAC в HTTP-ответ.
func TestConfig_SecretIsRedactedInEveryPrintForm(t *testing.T) {
	t.Parallel()

	cfg := session.DefaultConfig(testRealm, testSecret())
	raw := "rebar-auth-key!!"

	for name, printed := range map[string]string{
		"%v":  sprint("%v", cfg),
		"%+v": sprint("%+v", cfg),
		"%#v": sprint("%#v", cfg),
		"%s":  sprint("%s", cfg.Secret),
		"%x":  sprint("%x", cfg.Secret),
	} {
		assert.NotContainsf(t, printed, raw, "%s: секрет реалма утёк в печать", name)
		assert.NotContainsf(t, strings.ToLower(printed), "6b6579", "%s: байты ключа в hex", name)
	}
}

// Realm сервиса виден наружу: потребителю он нужен для куки и журнала.
func TestService_RealmIsExposed(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	assert.Equal(t, testRealm, st.svc.Realm())
	assert.Equal(t, auth.Realm("shop"), st.svc.Realm())
}
