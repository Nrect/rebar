package authhttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/token"
)

// НЕГОДНАЯ КУКА РОНЯЕТ ПРОЦЕСС НА СТАРТЕ. Половина этих ошибок иначе
// проявляется молча: браузер просто не ставит куку, вход не работает, и в
// логах ничего нет.
func TestCookieConfig_PanicsOnBadInvariants(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate func(*authhttp.CookieConfig)
		field  string
	}{
		"нулевая":            {func(c *authhttp.CookieConfig) { *c = authhttp.CookieConfig{} }, "CookieConfig.Name"},
		"без имени":          {func(c *authhttp.CookieConfig) { c.Name = "" }, "CookieConfig.Name"},
		"без имени CSRF":     {func(c *authhttp.CookieConfig) { c.CSRFName = "" }, "CookieConfig.CSRFName"},
		"одно имя на две":    {func(c *authhttp.CookieConfig) { c.CSRFName = c.Name }, "CookieConfig.CSRFName"},
		"без заголовка CSRF": {func(c *authhttp.CookieConfig) { c.CSRFHeader = "" }, "CookieConfig.CSRFHeader"},
		"SameSite по умолчанию": {
			func(c *authhttp.CookieConfig) { c.SameSite = http.SameSiteDefaultMode }, "CookieConfig.SameSite",
		},
		"None без Secure": {
			func(c *authhttp.CookieConfig) {
				c.Name, c.CSRFName = "shop", "shop_csrf"
				c.SameSite, c.Secure = http.SameSiteNoneMode, false
			}, "CookieConfig.Secure",
		},
		"__Host- без Secure": {
			func(c *authhttp.CookieConfig) { c.Secure = false }, "CookieConfig.Secure",
		},
		"__Host- с путём": {
			func(c *authhttp.CookieConfig) { c.Path = "/app" }, "CookieConfig.Path",
		},
		"__Host- с доменом": {
			func(c *authhttp.CookieConfig) { c.Domain = "shop.example" }, "CookieConfig.Domain",
		},
		"__Host- только у CSRF-куки": {
			func(c *authhttp.CookieConfig) { c.Name = "shop"; c.Domain = "shop.example" }, "CookieConfig.Domain",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := authhttp.DefaultCookieConfig("shop")
			tc.mutate(&cfg)

			defer func() {
				got := recover()
				require.NotNilf(t, got, "%s: конструктор обязан упасть", name)
				assert.Containsf(t, got, tc.field, "%s: паника обязана назвать поле", name)
			}()
			authhttp.Middleware(stubResolver{}, cfg, func(http.ResponseWriter, *http.Request, error) {})
		})
	}
}

func TestDefaultCookieConfig_IsValidAndStrict(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")

	assert.True(t, cfg.Secure)
	assert.Equal(t, "/", cfg.Path)
	assert.Empty(t, cfg.Domain)
	assert.Equal(t, http.SameSiteLaxMode, cfg.SameSite)
	assert.Equal(t, authhttp.DefaultCSRFHeader, cfg.CSRFHeader)
	assert.NotPanics(t, func() {
		authhttp.Middleware(stubResolver{}, cfg, func(http.ResponseWriter, *http.Request, error) {})
	})
}

// СЕССИОННАЯ КУКА HttpOnly ВСЕГДА, CSRF-КУКА — НИКОГДА: первая недоступна
// скрипту, потому что XSS не должен превращаться в кражу сессии; вторая обязана
// быть ему доступна, иначе double-submit нечем выполнить.
func TestSetSession_SetsBothCookiesWithTheRightFlags(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")
	rec := httptest.NewRecorder()
	expires := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)

	csrf, err := authhttp.SetSession(rec, cfg, "raw-session-token", expires)

	require.NoError(t, err)
	assert.Len(t, csrf, token.RawLen, "CSRF-токен — 256 бит из crypto/rand")

	cookies := cookiesByName(rec)
	session := cookies[cfg.Name]
	require.NotNil(t, session)
	assert.True(t, session.HttpOnly, "сессионная кука обязана быть HttpOnly")
	assert.True(t, session.Secure)
	assert.Equal(t, http.SameSiteLaxMode, session.SameSite)
	assert.Equal(t, "raw-session-token", session.Value)

	guard := cookies[cfg.CSRFName]
	require.NotNil(t, guard)
	assert.False(t, guard.HttpOnly, "CSRF-куку обязан прочитать фронтенд")
	assert.Equal(t, csrf, guard.Value)
}

// Каждый вызов даёт свой CSRF-токен: постоянное значение — это токен, который
// достаточно украсть один раз.
func TestSetSession_MintsAFreshCSRFTokenEveryTime(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")
	first, err := authhttp.SetSession(httptest.NewRecorder(), cfg, "raw", time.Now())
	require.NoError(t, err)
	second, err := authhttp.SetSession(httptest.NewRecorder(), cfg, "raw", time.Now())
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
}

// Стирание идёт ТЕМИ ЖЕ атрибутами: браузер сопоставляет куки по имени, пути и
// домену, и кука, стёртая с другим Path, продолжает жить.
func TestClearSession_ErasesBothWithMatchingAttributes(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")
	rec := httptest.NewRecorder()

	authhttp.ClearSession(rec, cfg)

	cookies := cookiesByName(rec)
	require.Len(t, cookies, 2)
	for name, c := range cookies {
		assert.Emptyf(t, c.Value, "%s: значение обязано быть стёрто", name)
		assert.Negativef(t, c.MaxAge, "%s: MaxAge обязан быть отрицательным", name)
		assert.Equalf(t, cfg.Path, c.Path, "%s: путь обязан совпасть с тем, чем ставили", name)
		assert.Equalf(t, cfg.Domain, c.Domain, "%s: домен обязан совпасть", name)
	}
	assert.True(t, cookies[cfg.Name].HttpOnly)
	assert.False(t, cookies[cfg.CSRFName].HttpOnly)
}

func cookiesByName(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}
