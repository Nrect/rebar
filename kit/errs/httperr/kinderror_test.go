package httperr_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// Класс без слага: статус по классу, слаг — имя класса. Таблица поимённо, как
// в TestStatusOf: сдвиг любой строки обязан быть виден в диффе теста.
func TestWrite_KindErrorAnswersByKind(t *testing.T) {
	t.Parallel()

	table := map[errs.Kind]struct {
		status int
		slug   string
		level  slog.Level
	}{
		errs.KindUnknown:         {http.StatusInternalServerError, "internal-server-error", slog.LevelError},
		errs.KindIncorrectInput:  {http.StatusBadRequest, "incorrect-input", slog.LevelDebug},
		errs.KindUnauthenticated: {http.StatusUnauthorized, "unauthenticated", slog.LevelWarn},
		errs.KindForbidden:       {http.StatusForbidden, "forbidden", slog.LevelWarn},
		errs.KindNotFound:        {http.StatusNotFound, "not-found", slog.LevelDebug},
		errs.KindConflict:        {http.StatusConflict, "conflict", slog.LevelDebug},
		errs.KindTooManyRequests: {http.StatusTooManyRequests, "too-many-requests", slog.LevelWarn},
		errs.KindPayloadTooLarge: {http.StatusRequestEntityTooLarge, "payload-too-large", slog.LevelDebug},
		errs.KindUnavailable:     {http.StatusServiceUnavailable, "unavailable", slog.LevelError},
		errs.KindTimeout:         {http.StatusGatewayTimeout, "timeout", slog.LevelError},
		errs.KindNotImplemented:  {http.StatusNotImplemented, "not-implemented", slog.LevelError},
	}
	require.Len(t, table, len(errs.AllKinds), "таблица и AllKinds разошлись")

	for _, kind := range errs.AllKinds {
		want, found := table[kind]
		require.Truef(t, found, "класс %q без строки в таблице", kind)

		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			// KindUnknown — отсутствие класса, то есть обычная ошибка.
			err := errors.New("pkg: unclassified")
			if kind != errs.KindUnknown {
				err = errs.Kinded(kind, "pkg: failed")
			}
			logger, handler := newLogger()
			rec := httptest.NewRecorder()

			httperr.New(httperr.Config{Logger: logger}).Write(t.Context(), rec, err)

			assert.Equal(t, want.status, rec.Code)
			assert.JSONEq(t, `{"slug":"`+want.slug+`"}`, rec.Body.String())
			assert.Empty(t, rec.Header().Get("Retry-After"), "задержка не выводится из статуса")
			record, ok := handler.only()
			require.True(t, ok, "ожидалась ровно одна запись лога")
			assert.Equal(t, want.level, record.Level)
		})
	}
}

// Текст и причина KindError — только в лог: пароль из причины не уходит ни в
// тело, ни в заголовки.
func TestWrite_KindErrorCauseNeverReachesClient(t *testing.T) {
	t.Parallel()

	const password = "hunter2"
	err := errs.Kinded(errs.KindUnavailable, "store: unavailable").
		WithCause(errors.New("dsn=postgres://user:" + password + "@host/db"))
	require.Contains(t, err.Error(), password, "утекать есть чему: пароль лежит в тексте ошибки")

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger, RequestID: staticID("req-7")}).Write(t.Context(), rec, err)

	body := rec.Body.String()
	assert.NotContains(t, body, password)
	assert.NotContains(t, body, "store", "текст KindError наружу тоже не уходит")
	assert.JSONEq(t, `{"slug":"unavailable","request_id":"req-7"}`, body)
	assert.NotContains(t, fmt.Sprint(rec.Header()), password)
}

// Translate первым: слаг и класс из словаря потребителя перекрывают имя класса,
// а ошибка без строки в словаре получает верный статус с техническим слагом.
func TestWrite_TranslateOverridesKindName(t *testing.T) {
	t.Parallel()

	errDenied := errs.Kinded(errs.KindForbidden, "entitlement: denied")
	calls := 0
	logger, _ := newLogger()
	responder := httperr.New(httperr.Config{
		Logger: logger,
		Translate: func(err error) error {
			calls++
			return errs.TranslateAs(err, errDenied, errs.NotFound("item-not-open"))
		},
	})

	rec := httptest.NewRecorder()
	responder.Write(t.Context(), rec, fmt.Errorf("open item: %w", errDenied))

	assert.Equal(t, 1, calls)
	assert.Equal(t, http.StatusNotFound, rec.Code, "класс потребителя старше класса пакета")
	assert.JSONEq(t, `{"slug":"item-not-open"}`, rec.Body.String())

	rec = httptest.NewRecorder()
	responder.Write(t.Context(), rec, errs.Kinded(errs.KindForbidden, "authz: denied"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"slug":"forbidden"}`, rec.Body.String())
}

// Нулевой KindError (var без Kinded) — не класс: 500 и InternalSlug, а не пустой слаг.
func TestWrite_ZeroKindErrorIsInternal(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := httptest.NewRecorder()

	httperr.New(httperr.Config{Logger: logger}).Write(t.Context(), rec, errs.KindError{})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"slug":"internal-server-error"}`, rec.Body.String())
}

// Страж ADR-0007: имя класса уходит слагом без таблицы умолчаний, поэтому
// каждое значение Kind обязано быть годным слагом.
func TestKindNamesAreValidSlugs(t *testing.T) {
	t.Parallel()

	for _, kind := range errs.AllKinds {
		assert.Truef(t, errs.ValidSlug(string(kind)), "Kind %q не годится в слаг ответа", kind)
	}
}
