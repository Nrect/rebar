package monolith_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/kit/config"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"

	"github.com/nrect/rebar/examples/monolith"
)

// testSecret — секрет реалма прогона. Не из окружения: тест обязан быть
// воспроизводимым, а секрет здесь ничего не защищает.
const testSecret = "монолит-секрет-достаточной-длины-для-hmac"

// stand — приложение прогона со своим сервером.
type stand struct {
	app    *monolith.App
	srv    *httptest.Server
	client *http.Client
	dsn    string
	db     *pgxpool.Pool
	// setCookies — куки ПОСЛЕДНЕГО ответа целиком. Из jar их взять нельзя:
	// он хранит имя и значение, а проверять надо HttpOnly.
	setCookies []*http.Cookie
}

// newStand поднимает приложение на общей базе и общем Mailpit.
//
// БАЗА У КАЖДОГО ТЕСТА СВОЯ (pgtest.Schema): сквозной тест пишет во все
// таблицы сразу, и общая база сделала бы порядок тестов частью результата.
func newStand(t *testing.T) *stand {
	t.Helper()
	return newStandWith(t, nil)
}

// newStandWith — то же, но с правкой окружения: часть тестов меняет политику
// (например TTL снимка прав), и делать это конфигурацией честнее, чем
// подменой внутренностей.
func newStandWith(t *testing.T, overrides map[string]string) *stand {
	t.Helper()
	app, dsn := buildApp(t, overrides)

	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &stand{app: app, srv: srv, client: &http.Client{Jar: jar}, dsn: dsn}
}

// buildApp собирает приложение и падает на ошибке сборки.
func buildApp(t *testing.T, overrides map[string]string) (app *monolith.App, dsn string) {
	t.Helper()
	app, dsn, err := tryBuildApp(t, overrides)
	require.NoError(t, err, "сборка приложения")
	return app, dsn
}

// tryBuildApp — то же, но ошибку отдаёт вызывающему: часть тестов проверяет
// ИМЕННО отказ на старте.
func tryBuildApp(t *testing.T, overrides map[string]string) (app *monolith.App, dsn string, err error) {
	t.Helper()
	pgtest.Short(t)

	dsn = schemaDSN(t)
	env := map[string]string{
		"DATABASE_URL": dsn,
		"AUTH_SECRET":  testSecret,
		"SMTP_HOST":    box.host,
		"SMTP_PORT":    strconv.Itoa(box.smtp),
		"FILES_DIR":    t.TempDir(),
		"TICK":         "1h", // задачи гоняем руками: RunNow, а не по расписанию
	}
	for k, v := range overrides {
		env[k] = v
	}
	cfg, err := monolith.Load(config.New(func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}))
	if err != nil {
		return nil, dsn, err
	}
	if app, err = monolith.New(t.Context(), cfg, monolith.Migrations()); err != nil {
		return nil, dsn, err
	}
	t.Cleanup(func() { _ = app.Close(context.WithoutCancel(t.Context())) })
	return app, dsn, nil
}

// schemaDSN — своя схема на тест и DSN с search_path в неё.
//
// Написан здесь, а не взят из pgtest: Schema отдаёт ПУЛ, а приложению нужна
// строка соединения — оно поднимает пул само (doc.go, «Что не сошлось»).
func schemaDSN(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	name := "t" + hex.EncodeToString(buf)

	_, err = db.Pool().Exec(t.Context(), "CREATE SCHEMA "+name)
	require.NoError(t, err, "CREATE SCHEMA %s", name)

	dsn, err := postgres.WithRuntimeParam(db.DSN(), "search_path", name)
	require.NoError(t, err)
	return dsn
}

// postJSON шлёт JSON и отдаёт статус с разобранным телом.
func (s *stand) postJSON(t *testing.T, path string, body any) (status int, out map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return s.do(t, http.MethodPost, path, "application/json", bytes.NewReader(raw))
}

// get — GET без тела.
func (s *stand) get(t *testing.T, path string) (status int, out map[string]any) {
	t.Helper()
	return s.do(t, http.MethodGet, path, "", nil)
}

func (s *stand) do(t *testing.T, method, path, contentType string, body io.Reader) (status int, out map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.srv.URL+path, body)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	s.csrf(req)
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	if c := resp.Cookies(); len(c) > 0 {
		s.setCookies = c
	}
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out = map[string]any{}
	if len(bytes.TrimSpace(payload)) > 0 {
		_ = json.Unmarshal(payload, &out)
	}
	out["__raw"] = string(payload)
	return resp.StatusCode, out
}

// csrf проставляет заголовок из CSRF-куки. Кука отдельная и НЕ HttpOnly
// намеренно: сравнивать нечего, если её не видит клиентский код.
func (s *stand) csrf(req *http.Request) {
	base, err := url.Parse(s.srv.URL)
	if err != nil {
		return
	}
	_, csrfName := s.app.CookieNames()
	for _, c := range s.client.Jar.Cookies(base) {
		if c.Name == csrfName {
			req.Header.Set(authhttp.DefaultCSRFHeader, c.Value)
		}
	}
}

// str — строковое поле ответа. Приведение с проверкой: тест, падающий
// паникой вместо внятного сообщения, отнимает разбор вместо того, чтобы его
// дать.
func str(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(string)
	require.True(t, ok, "поле %q не строка; тело: %s", key, raw(body))
	return value
}

// boolOf — булево поле ответа.
func boolOf(t *testing.T, body map[string]any, key string) bool {
	t.Helper()
	value, ok := body[key].(bool)
	require.True(t, ok, "поле %q не булево; тело: %s", key, raw(body))
	return value
}

// raw — тело ответа как строка: по нему проверяется, что наружу не уехало
// лишнего.
func raw(body map[string]any) string {
	s, _ := body["__raw"].(string)
	return s
}

// linkToken — токен из ссылки письма. Сырой токен живёт только в письме: в
// базе лежит его HMAC.
var linkToken = regexp.MustCompile(`token=([A-Za-z0-9_\-]+)`)

func tokenFromLetter(t *testing.T, text string) string {
	t.Helper()
	m := linkToken.FindStringSubmatch(text)
	require.Len(t, m, 2, "в письме нет ссылки с токеном: %q", text)
	value, err := url.QueryUnescape(m[1])
	require.NoError(t, err)
	require.NotEmpty(t, value)
	return value
}

// hashOf — HMAC токена под секретом реалма: то, что обязано лежать в базе.
func hashOf(t *testing.T, rawToken string) string {
	t.Helper()
	secret, err := token.NewSecret([]byte(testSecret))
	require.NoError(t, err)
	return token.Hash(rawToken, secret)
}
