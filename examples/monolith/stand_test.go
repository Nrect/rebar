package monolith_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
)

// pool — пул к схеме теста. Тест смотрит в базу напрямую: утверждение «легло
// одной транзакцией» проверяется строками, а не ответом ручки.
func (s *stand) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if s.db == nil {
		p, err := pgxpool.New(t.Context(), s.dsn)
		require.NoError(t, err)
		t.Cleanup(p.Close)
		s.db = p
	}
	return s.db
}

// requireCount — сколько строк удовлетворяет запросу.
func requireCount(t *testing.T, s *stand, want int, query string, args ...any) {
	t.Helper()
	var got int
	err := s.pool(t).QueryRow(t.Context(), query, args...).Scan(&got)
	require.NoError(t, err, "запрос: %s", query)
	require.Equal(t, want, got, "запрос: %s", query)
}

// runJob гоняет фоновую задачу разово. Планировщик в тестах не стартует:
// расписание сделало бы результат зависящим от времени прогона.
func (s *stand) runJob(t *testing.T, name string) int {
	t.Helper()
	processed, err := s.app.Jobs().RunNow(t.Context(), name)
	require.NoError(t, err, "задача %s", name)
	return processed
}

// grantRole выдаёт роль покупателя. Роли читаются на КАЖДУЮ проверку, поэтому
// выдать её достаточно один раз и до входа.
func (s *stand) grantRole(t *testing.T, subject uuid.UUID) {
	t.Helper()
	store := authzpg.New(s.pool(t))
	err := store.Assign(t.Context(), authzpg.Assignment{
		Subject:   authz.Subject{Realm: "shop", ID: subject.String()},
		Role:      "customer",
		GrantedBy: "test",
		GrantedAt: time.Now().UTC(),
	})
	require.NoError(t, err, "выдача роли")
}

// cookies — сессионная и CSRF-кука так, как их выставил сервер.
//
// Берутся из Set-Cookie ПОСЛЕДНЕГО ответа, а не из jar: jar хранит имя и
// значение, а проверять надо атрибуты — HttpOnly в первую очередь.
func (s *stand) cookies(t *testing.T) (session, csrf *http.Cookie) {
	t.Helper()
	sessionName, csrfName := s.app.CookieNames()
	for _, c := range s.setCookies {
		switch c.Name {
		case sessionName:
			session = c
		case csrfName:
			csrf = c
		}
	}
	require.NotNil(t, session, "сессионная кука выставлена")
	require.NotNil(t, csrf, "кука CSRF выставлена отдельно")
	return session, csrf
}

// upload шлёт файл multipart'ом.
func (s *stand) upload(t *testing.T, filename string, body []byte) (status int, out map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(body)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return s.do(t, http.MethodPost, "/upload", w.FormDataContentType(), &buf)
}

// scrape — тело /metrics.
func (s *stand) scrape(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.srv.URL+"/metrics", http.NoBody)
	require.NoError(t, err)
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return string(payload)
}

// replayOutbox возвращает разобранное событие в работу: так проверяется
// идемпотентность хендлера при повторной доставке (at-least-once).
func (s *stand) replayOutbox(t *testing.T) error {
	t.Helper()
	_, err := s.pool(t).Exec(t.Context(),
		`UPDATE outbox_messages SET status = 'pending', done_at = NULL, available_at = now()
		 WHERE kind = 'order.paid'`)
	if err != nil {
		return err
	}
	s.runJob(t, "outbox_drain")
	return nil
}

// pngBody — минимальная настоящая картинка: тип определяется ПО СОДЕРЖИМОМУ,
// и подделать его заголовком клиента нельзя.
func pngBody() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("тест: png не собрался: " + err.Error())
	}
	return buf.Bytes()
}
