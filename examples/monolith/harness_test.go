package monolith_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/kit/config"
	"github.com/nrect/rebar/postgres/pgtest"

	"github.com/nrect/rebar/examples/monolith"
	"github.com/nrect/rebar/examples/monolith/logotel"
)

// testSecret — секрет реалма прогона. Не из окружения: тест обязан быть
// воспроизводимым, а секрет здесь ничего не защищает.
const testSecret = "монолит-секрет-достаточной-длины-для-hmac"

// stand — приложение прогона со своими серверами.
type stand struct {
	app *monolith.App
	// srv — публичные ручки, probes — служебные; оба без портов процесса.
	srv    *httptest.Server
	probes *httptest.Server
	client *http.Client
	dsn    string
	db     *pgxpool.Pool
	// log — логгер процесса поверх logs: тест читает то, что записали блоки.
	log  *slog.Logger
	logs *logBuffer
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
	s, err := tryStand(t, overrides)
	require.NoError(t, err, "сборка приложения")
	return s
}

// tryStand — то же, но ошибку сборки отдаёт вызывающему: часть тестов
// проверяет ИМЕННО отказ на старте.
func tryStand(t *testing.T, overrides map[string]string) (*stand, error) {
	t.Helper()
	pgtest.Short(t)

	dsn := pgtest.SchemaDSN(t, db)
	cfg, err := monolith.Load(loaderOf(standEnv(t, dsn), overrides))
	if err != nil {
		return nil, err
	}
	return tryServe(t, cfg, dsn)
}

// newReplica — вторая реплика приложения на базе стенда s: свои пул,
// планировщик, провайдер и /metrics, схема та же. Так тест изображает два
// инстанса одного сервиса.
func newReplica(t *testing.T, s *stand) *stand {
	t.Helper()
	r, err := tryServe(t, loadConfig(t, s.dsn, nil), s.dsn)
	require.NoError(t, err, "сборка реплики")
	return r
}

// tryServe собирает приложение по конфигу и ставит перед ним серверы.
func tryServe(t *testing.T, cfg monolith.Config, dsn string) (*stand, error) {
	t.Helper()
	logs := &logBuffer{}
	log := logotel.New(logs, slog.LevelDebug)
	app, err := monolith.New(t.Context(), cfg, log, monolith.Migrations())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = app.Close(context.WithoutCancel(t.Context())) })

	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	probes := httptest.NewServer(app.Probes())
	t.Cleanup(probes.Close)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &stand{
		app: app, srv: srv, probes: probes, client: &http.Client{Jar: jar},
		dsn: dsn, log: log, logs: logs,
	}, nil
}

// loaderOf — Loader поверх окружения прогона с правками теста.
func loaderOf(env, overrides map[string]string) *config.Loader {
	for k, v := range overrides {
		env[k] = v
	}
	return config.New(func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
}

// loadConfig — конфиг прогона; ошибку не терпит.
func loadConfig(t *testing.T, dsn string, overrides map[string]string) monolith.Config {
	t.Helper()
	cfg, err := monolith.Load(loaderOf(standEnv(t, dsn), overrides))
	require.NoError(t, err, "конфиг прогона")
	return cfg
}

// standEnv — окружение прогона: stand.env, с которым стенд поднимается по
// README, а поверх — общая база, общий Mailpit и свой каталог файлов. Такты —
// час: планировщик либо не стартует, либо не успевает тикнуть, и задачи
// гоняются руками (RunNow) — иначе результат зависел бы от времени прогона.
// Порты — :0: номер выбирает система, и прогоны порт не делят.
func standEnv(t *testing.T, dsn string) map[string]string {
	t.Helper()
	env := envFile(t, "stand.env")
	for k, v := range map[string]string{
		"DATABASE_URL":  dsn,
		"AUTH_SECRET":   testSecret,
		"SMTP_HOST":     box.host,
		"SMTP_PORT":     strconv.Itoa(box.smtp),
		"FILES_DIR":     t.TempDir(),
		"TICK":          "1h",
		"GAUGES_TICK":   "1h",
		"ADDR":          "127.0.0.1:0",
		"INTERNAL_ADDR": "127.0.0.1:0",
	} {
		env[k] = v
	}
	return env
}

// envFile читает файл окружения: строки KEY=VALUE, пустые и # пропускаются.
func envFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	env := map[string]string{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "%s: строка без «=»: %q", path, line)
		env[key] = value
	}
	require.NoError(t, lines.Err())
	return env
}

// buildAppFromConfig собирает приложение с режимом транспорта, выставленным
// РУКАМИ, минуя Loader.
//
// Нужен ровно затем, чтобы проверить второй рубеж: из окружения негодное
// значение не проходит (Loader.Enum), и без этой калитки паника сборки была
// бы недостижима, то есть страж молчал бы всегда.
func buildAppFromConfig(t *testing.T, mode monolith.TransportMode) {
	t.Helper()
	pgtest.Short(t)

	cfg := loadConfig(t, pgtest.SchemaDSN(t, db), nil)
	cfg.Transport = mode
	app, err := monolith.New(t.Context(), cfg, slog.New(slog.DiscardHandler), monolith.Migrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Close(context.WithoutCancel(t.Context())) })
}

// logBuffer — записи логгера прогона. Под замком: пишут горутины сервера и
// задач, а читает тест.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records — записи с сообщением msg. JSON-обработчик пишет запись одним
// Write, поэтому строка буфера — целая запись.
func (b *logBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(b.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "запись лога не JSON: %s", line)
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
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
