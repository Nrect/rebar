package monolith_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/examples/monolith"
)

// TestStart_ServersHaveAllTimeouts — у обоих серверов процесса все четыре срока
// соединения (docs/CONSUMER.md, §9, п. 1): без любого медленный клиент держит
// соединение сколько хочет.
func TestStart_ServersHaveAllTimeouts(t *testing.T) {
	s := newStand(t)
	startProcess(t, s)
	public, internal := s.app.Servers()

	for _, srv := range []struct {
		name string
		srv  *http.Server
	}{{"public", public}, {"internal", internal}} {
		for _, d := range []struct {
			field string
			value time.Duration
		}{
			{"ReadHeaderTimeout", srv.srv.ReadHeaderTimeout},
			{"ReadTimeout", srv.srv.ReadTimeout},
			{"WriteTimeout", srv.srv.WriteTimeout},
			{"IdleTimeout", srv.srv.IdleTimeout},
		} {
			assert.Positive(t, d.value, "%s: %s", srv.name, d.field)
		}
	}
}

// TestJSON_OneValueUnderCeiling — тело ручки — одно значение JSON под потолком:
// после него только пробелы, превышение потолка — 413, а не 400.
func TestJSON_OneValueUnderCeiling(t *testing.T) {
	s := newStand(t)
	valid := `{"Login":"` + testLogin + `","Password":"` + testPassword + `"}`

	for _, c := range []struct {
		name, body string
		status     int
		slug       string
	}{
		{"мусор после объекта", valid + " мусор", http.StatusBadRequest, "body-invalid"},
		{"два объекта подряд", valid + "{}", http.StatusBadRequest, "body-invalid"},
		{"тело выше потолка", `{"Login":"` + strings.Repeat("a", monolith.MaxJSONBytes) + `"}`,
			http.StatusRequestEntityTooLarge, "body-too-large"},
		{"одно значение и пробелы", valid + " \n\t", http.StatusAccepted, ""},
	} {
		status, body := s.do(t, http.MethodPost, "/register", "application/json", strings.NewReader(c.body))
		assert.Equal(t, c.status, status, "%s: %s", c.name, raw(body))
		if c.slug != "" {
			assert.Equal(t, c.slug, body["slug"], "%s: слаг", c.name)
		}
	}
}

// TestAPIHeaders_OnEveryResponse — nosniff и no-store на каждом ответе
// публичного обработчика: ручки, отказа ручки, отказа сессии и 404 роутера.
func TestAPIHeaders_OnEveryResponse(t *testing.T) {
	s := newStand(t)
	valid := `{"Login":"` + testLogin + `","Password":"` + testPassword + `"}`

	for _, c := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"ответ ручки", http.MethodPost, "/register", valid, http.StatusAccepted},
		{"отказ ручки", http.MethodPost, "/register", "{", http.StatusBadRequest},
		{"отказ сессии", http.MethodGet, "/lesson/lesson-01", "", http.StatusUnauthorized},
		{"404 роутера", http.MethodGet, "/nope", "", http.StatusNotFound},
	} {
		status, header := headersOf(t, s, c.method, c.path, c.body)
		require.Equal(t, c.status, status, c.name)
		assert.Equal(t, "nosniff", header.Get("X-Content-Type-Options"), "%s: X-Content-Type-Options", c.name)
		assert.Equal(t, "no-store", header.Get("Cache-Control"), "%s: Cache-Control", c.name)
	}
}

// TestUpload_OverCeilingIs413 — тело загрузки выше потолка отвечает 413: multipart
// не прячет MaxBytesError за своей ошибкой.
func TestUpload_OverCeilingIs413(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))

	status, body := s.upload(t, "big.png", bytes.Repeat([]byte("a"), monolith.MaxUploadBytes))
	require.Equal(t, http.StatusRequestEntityTooLarge, status, "загрузка выше потолка: %s", raw(body))
	require.Equal(t, "body-too-large", body["slug"], "загрузка выше потолка: слаг")
}

// slowTimeout — сроки чтения и записи сервера в тесте медленной загрузки.
const slowTimeout = 500 * time.Millisecond

// TestUpload_SlowBodyOutlivesReadTimeout — ручка загрузки продлевает сроки
// соединения: тело идёт втрое дольше ReadTimeout и WriteTimeout сервера, а файл
// принят и ответ дошёл.
func TestUpload_SlowBodyOutlivesReadTimeout(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))

	slow := httptest.NewUnstartedServer(s.app.Handler())
	slow.Config.ReadTimeout = slowTimeout
	slow.Config.WriteTimeout = slowTimeout
	slow.Start()
	t.Cleanup(slow.Close)

	body, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { pw.CloseWithError(writeSlowly(mw, pngBody(), 6)) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, slow.URL+"/upload", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	s.csrf(req)
	resp, err := s.client.Do(req)
	require.NoError(t, err, "ответ не дошёл: срок соединения не продлён")
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "срок чтения не продлён: %s", payload)
}

// writeSlowly пишет файл частями с паузой в половину slowTimeout.
func writeSlowly(mw *multipart.Writer, content []byte, parts int) error {
	part, err := mw.CreateFormFile("file", "slow.png")
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(content, (len(content)+parts-1)/parts) {
		time.Sleep(slowTimeout / 2)
		if _, err := part.Write(chunk); err != nil {
			return err
		}
	}
	return mw.Close()
}

// headersOf — статус и заголовки ответа публичного обработчика.
func headersOf(t *testing.T, s *stand, method, path, body string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header
}
