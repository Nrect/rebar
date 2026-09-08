package httperr_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
)

var errUserNotFound = errs.NotFound("user-not-found")

func staticID(id string) func(context.Context) string {
	return func(context.Context) string { return id }
}

func TestNew_PanicsOnBadInternalSlug(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		`httperr.New: Config.InternalSlug "Oops" must be a valid slug (errs.ValidSlug)`,
		func() { httperr.New(httperr.Config{InternalSlug: "Oops"}) })
	assert.NotPanics(t, func() { httperr.New(httperr.Config{}) })
}

// Ответ на SlugError: статус по Kind, плоское тело, заголовки.
func TestWrite_SlugError(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	responder := httperr.New(httperr.Config{Logger: logger, RequestID: staticID("req-1")})
	rec := httptest.NewRecorder()

	responder.Write(t.Context(), rec, errUserNotFound.WithCause(errors.New("sql: no rows in result set")))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"slug":"user-not-found","request_id":"req-1"}`, rec.Body.String())
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("Retry-After"))
	assert.NotContains(t, rec.Body.String(), "sql:", "причина наружу не уходит")
}

// SlugError сквозь обёртку fmt.Errorf: статус и слаг те же.
func TestWrite_WrappedSlugError(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).
		Write(t.Context(), rec, fmt.Errorf("get profile: %w", errUserNotFound))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"slug":"user-not-found"}`, rec.Body.String())
}

// Без RequestID поле опускается — пустого request_id в теле не бывает.
func TestWrite_RequestIDOmitted(t *testing.T) {
	t.Parallel()

	for name, requestID := range map[string]func(context.Context) string{
		"порт не задан": nil,
		"пустой id":     staticID(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logger, _ := newLogger()
			rec := httptest.NewRecorder()

			httperr.New(httperr.Config{Logger: logger, RequestID: requestID}).
				Write(t.Context(), rec, errUserNotFound)

			assert.JSONEq(t, `{"slug":"user-not-found"}`, rec.Body.String())
		})
	}
}

// Чужая ошибка — 500 и слаг-заглушка; текст наружу не уходит никогда.
func TestWrite_ForeignErrorHidesText(t *testing.T) {
	t.Parallel()

	logger, handler := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).
		Write(t.Context(), rec, errors.New("pq: relation \"users\" does not exist"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"slug":"internal-server-error"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "users")

	record, ok := handler.only()
	require.True(t, ok)
	value, ok := attrOf(record, "error")
	require.True(t, ok, "причина обязана быть в логе")
	assert.Contains(t, fmt.Sprint(value.Any()), "relation")
}

// Слаг у собранной вручную SlugError конструктор не проходил: наружу его не пускаем.
func TestWrite_HandBuiltSlugErrorWithBadSlugIsInternal(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).
		Write(t.Context(), rec, errs.SlugError{Slug: `not a slug"`, Kind: errs.KindNotFound})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"slug":"internal-server-error"}`, rec.Body.String())
}

func TestWrite_CustomInternalSlug(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger, InternalSlug: "oops"}).
		Write(t.Context(), rec, errors.New("boom"))

	assert.JSONEq(t, `{"slug":"oops"}`, rec.Body.String())
}

func TestWrite_NilErrorWritesNothing(t *testing.T) {
	t.Parallel()

	logger, handler := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).Write(t.Context(), rec, nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Empty(t, handler.all())
}

// Translate применяется первым и ровно один раз.
func TestWrite_Translate(t *testing.T) {
	t.Parallel()

	errBusy := errors.New("auth: busy")
	calls := 0
	logger, _ := newLogger()
	responder := httperr.New(httperr.Config{
		Logger: logger,
		Translate: func(err error) error {
			calls++
			return errs.TranslateAs(err, errBusy, errs.TooManyRequests("too-many-attempts"))
		},
	})
	rec := httptest.NewRecorder()

	responder.Write(t.Context(), rec, fmt.Errorf("login: %w", errBusy))

	assert.Equal(t, 1, calls)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.JSONEq(t, `{"slug":"too-many-attempts"}`, rec.Body.String())
}

func TestWrite_RetryAfter(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"нет контракта":    {errUserNotFound, ""},
		"контракт отказал": {retryable{error: errUserNotFound, delay: time.Minute, ok: false}, ""},
		"секунды":          {retryable{error: errUserNotFound, delay: 30 * time.Second, ok: true}, "30"},
		"округление вверх": {retryable{error: errUserNotFound, delay: 1500 * time.Millisecond, ok: true}, "2"},
		"нулевая задержка": {retryable{error: errUserNotFound, delay: 0, ok: true}, "0"},
		"сквозь обёртку":   {fmt.Errorf("wrap: %w", retryable{error: errUserNotFound, delay: time.Minute, ok: true}), "60"},
		"чужая без слага":  {retryable{error: errors.New("boom"), delay: 5 * time.Second, ok: true}, "5"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logger, _ := newLogger()
			rec := httptest.NewRecorder()

			httperr.New(httperr.Config{Logger: logger}).Write(t.Context(), rec, tc.err)

			assert.Equal(t, tc.want, rec.Header().Get("Retry-After"))
		})
	}
}

// Уровень лога — по классу ответа: иначе сканер портов заливает Error-лог.
func TestWrite_LogLevels(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err   error
		level slog.Level
	}{
		"500 внутренняя": {errors.New("boom"), slog.LevelError},
		"503 недоступно": {errs.Unavailable("service-unavailable"), slog.LevelError},
		"401 без входа":  {errs.Unauthenticated("unauthorized"), slog.LevelWarn},
		"403 без прав":   {errs.Forbidden("forbidden"), slog.LevelWarn},
		"429 лимит":      {errs.TooManyRequests("too-many-requests"), slog.LevelWarn},
		"404 не найдено": {errUserNotFound, slog.LevelDebug},
		"400 не прошло":  {errs.IncorrectInput("validation-error"), slog.LevelDebug},
		"409 конфликт":   {errs.Conflict("conflict"), slog.LevelDebug},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logger, handler := newLogger()
			httperr.New(httperr.Config{Logger: logger, RequestID: staticID("req-9")}).
				Write(t.Context(), httptest.NewRecorder(), tc.err)

			record, ok := handler.only()
			require.True(t, ok, "ожидалась ровно одна запись лога")
			assert.Equal(t, tc.level, record.Level)

			slug, ok := attrOf(record, "slug")
			assert.True(t, ok)
			assert.NotEmpty(t, slug.String())
			id, ok := attrOf(record, "request_id")
			assert.True(t, ok)
			assert.Equal(t, "req-9", id.String())
		})
	}
}

// Клиент ушёл — это не сбой сервера: лога нет, тело всё равно пишется.
func TestWrite_CanceledContextIsNotLogged(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	logger, handler := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).Write(ctx, rec, errors.New("boom"))

	assert.Empty(t, handler.all())
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// Заголовки уже ушли: второй статус и второе тело сломали бы ответ.
func TestWrite_ResponseAlreadyStarted(t *testing.T) {
	t.Parallel()

	for name, wrap := range map[string]func(http.ResponseWriter) http.ResponseWriter{
		"обёртка с Written()": func(w http.ResponseWriter) http.ResponseWriter {
			return startedWriter{ResponseWriter: w, written: true}
		},
		"обёртка с Status()": func(w http.ResponseWriter) http.ResponseWriter {
			return statusWriter{ResponseWriter: w, status: http.StatusOK}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logger, handler := newLogger()
			rec := httptest.NewRecorder()

			httperr.New(httperr.Config{Logger: logger}).Write(t.Context(), wrap(rec), errUserNotFound)

			assert.Empty(t, rec.Body.String(), "тело не пишется дважды")
			assert.Empty(t, rec.Header().Get("Content-Type"))
			assert.Len(t, handler.all(), 1, "лог остаётся")
		})
	}
}

// Обёртка, которая ещё не писала, ответу не мешает.
func TestWrite_UnstartedWrapperIsWritten(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).
		Write(t.Context(), statusWriter{ResponseWriter: rec, status: 0}, errUserNotFound)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"slug":"user-not-found"}`, rec.Body.String())
}
