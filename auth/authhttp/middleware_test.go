package authhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/session"
)

const (
	sessionToken = "raw-session-token"
	csrfToken    = "dGhpcy1pcy1hLWNzcmYtdG9rZW4"
)

// stubResolver — разбор куки с заданным исходом. Не мок: он моделирует ответ
// сервиса, а не запоминает вызовы.
type stubResolver struct {
	subject uuid.UUID
	err     error
}

func (s stubResolver) Resolve(_ context.Context, raw string) (auth.Principal, error) {
	if s.err != nil {
		return auth.Principal{}, s.err
	}
	if raw != sessionToken {
		return auth.Principal{}, session.ErrNoSession
	}
	return auth.Principal{Realm: "shop", SubjectID: s.subject, SessionHash: "hash-of-" + raw}, nil
}

func TestMiddleware_PutsPrincipalIntoContext(t *testing.T) {
	t.Parallel()

	subject := uuid.New()
	rec, seen := serve(t, stubResolver{subject: subject}, request(t, http.MethodGet, true, true))

	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, seen.principal)
	assert.Equal(t, subject, seen.principal.SubjectID)
	assert.Equal(t, auth.Realm("shop"), seen.principal.Realm)
	assert.Equal(t, "hash-of-"+sessionToken, seen.principal.SessionHash,
		"в контекст уезжает хэш сессии, а не сырой токен")
}

// Без куки — ErrNoSession и ни одного круга в базу: Deny потребителя ответит
// 401, а хранилище об этом запросе не узнает.
func TestMiddleware_DeniesWithoutCookie(t *testing.T) {
	t.Parallel()

	req := request(t, http.MethodGet, false, false)
	rec, seen := serve(t, stubResolver{}, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	require.ErrorIs(t, seen.denied, session.ErrNoSession)
	assert.Nil(t, seen.principal, "хендлер не должен был получить управление")
}

func TestMiddleware_DeniesEmptyCookie(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.AddCookie(&http.Cookie{Name: authhttp.DefaultCookieConfig("shop").Name, Value: ""})
	rec, seen := serve(t, stubResolver{}, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	require.ErrorIs(t, seen.denied, session.ErrNoSession)
}

// НА НЕБЕЗОПАСНЫХ МЕТОДАХ CSRF ОБЯЗАТЕЛЕН, и проверка идёт ДО разбора сессии:
// подделанный межсайтовый запрос несёт куку, но не несёт заголовка, и отбить
// его надо, не тратя соединение из пула.
func TestMiddleware_ChecksCSRFBeforeResolvingSession(t *testing.T) {
	t.Parallel()

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PURGE",
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			rec, seen := serve(t, stubResolver{}, request(t, method, true, false))

			assert.Equal(t, http.StatusForbidden, rec.Code)
			require.ErrorIs(t, seen.denied, authhttp.ErrCSRF)
			assert.Nil(t, seen.principal)
		})
	}
}

func TestMiddleware_RefusesMismatchedCSRF(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")
	for name, header := range map[string]string{
		"пустой заголовок": "",
		"чужое значение":   "another-token-entirely",
		"обрезанное":       csrfToken[:len(csrfToken)-1],
		"с довеском":       csrfToken + "x",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := request(t, http.MethodPost, true, true)
			req.Header.Set(cfg.CSRFHeader, header)
			rec, seen := serve(t, stubResolver{}, req)

			assert.Equal(t, http.StatusForbidden, rec.Code)
			require.ErrorIs(t, seen.denied, authhttp.ErrCSRF)
		})
	}
}

// Безопасные методы CSRF не требуют: GET не меняет состояния, и требовать на
// нём заголовок значит ломать переход по ссылке.
func TestMiddleware_SafeMethodsSkipCSRF(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			rec, seen := serve(t, stubResolver{subject: uuid.New()}, request(t, method, true, false))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.NotNil(t, seen.principal)
		})
	}
}

func TestMiddleware_PassesSessionErrorsToDeny(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"сессии нет":           {session.ErrNoSession, http.StatusUnauthorized},
		"хранилище недоступно": {auth.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec, seen := serve(t, stubResolver{err: tc.err}, request(t, http.MethodGet, true, true))

			assert.Equal(t, tc.want, rec.Code)
			require.ErrorIs(t, seen.denied, tc.err, "причину называет пакет, статус выбирает потребитель")
		})
	}
}

func TestMiddleware_PanicsOnMissingWiring(t *testing.T) {
	t.Parallel()

	cfg := authhttp.DefaultCookieConfig("shop")
	assert.Panics(t, func() { authhttp.Middleware(nil, cfg, denyAll) })
	assert.Panics(t, func() { authhttp.Middleware(stubResolver{}, cfg, nil) })
}

// Пустой принципал наружу не выдаётся: нулевое значение, принятое за
// принципала, — это доступ от лица uuid.Nil.
func TestPrincipalFrom_RefusesZeroPrincipal(t *testing.T) {
	t.Parallel()

	_, ok := authhttp.PrincipalFrom(t.Context())
	assert.False(t, ok, "контекст без сессии не должен отдавать принципала")

	_, ok = authhttp.PrincipalFrom(authhttp.WithPrincipal(t.Context(), auth.Principal{}))
	assert.False(t, ok, "нулевой принципал — это отсутствие принципала")

	want := auth.Principal{Realm: "shop", SubjectID: uuid.New(), SessionHash: "h"}
	got, ok := authhttp.PrincipalFrom(authhttp.WithPrincipal(t.Context(), want))
	require.True(t, ok)
	assert.Equal(t, want, got)
}

// seenState — что увидели хендлер и Deny.
type seenState struct {
	principal *auth.Principal
	denied    error
}

// serve прогоняет запрос через middleware с боевым Deny-переходником:
// статусы выбирает ПОТРЕБИТЕЛЬ, и тест показывает, как он это делает.
func serve(t *testing.T, res authhttp.Resolver, req *http.Request) (*httptest.ResponseRecorder, *seenState) {
	t.Helper()

	seen := &seenState{}
	deny := func(w http.ResponseWriter, _ *http.Request, err error) {
		seen.denied = err
		w.WriteHeader(statusOf(err))
	}
	handler := authhttp.Middleware(res, authhttp.DefaultCookieConfig("shop"), deny)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := authhttp.PrincipalFrom(r.Context()); ok {
				seen.principal = &p
			}
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, seen
}

// statusOf — то, что у потребителя делает kit/errs/httperr: пакет называет
// причину, статус выбирает его API.
func statusOf(err error) int {
	switch {
	case errors.Is(err, authhttp.ErrCSRF):
		return http.StatusForbidden
	case errors.Is(err, auth.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnauthorized
	}
}

func denyAll(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusUnauthorized) }

// request — запрос с куками по вкусу: withSession кладёт сессионную,
// withCSRF — CSRF-куку И совпадающий заголовок.
func request(t *testing.T, method string, withSession, withCSRF bool) *http.Request {
	t.Helper()

	cfg := authhttp.DefaultCookieConfig("shop")
	req := httptest.NewRequest(method, "/", http.NoBody)
	if withSession {
		req.AddCookie(&http.Cookie{Name: cfg.Name, Value: sessionToken})
	}
	if withCSRF {
		req.AddCookie(&http.Cookie{Name: cfg.CSRFName, Value: csrfToken})
		req.Header.Set(cfg.CSRFHeader, csrfToken)
	}
	return req
}
