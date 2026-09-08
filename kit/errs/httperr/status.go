package httperr

import (
	"fmt"
	"net/http"

	"github.com/nrect/rebar/kit/errs"
)

// DefaultInternalSlug — слаг ответа на всё, что не errs.SlugError.
const DefaultInternalSlug = "internal-server-error"

// statusByKind — единственная таблица «класс → статус». Полноту держит
// errstest.KindStatusTable.
//
// Unavailable — 503, а НЕ 502: 502 означает «шлюз получил негодный ответ», и
// клиенты с провайдерами повторяют по 503 иначе. Timeout — 504 по той же
// причине: он повторяем, а 500 нет.
var statusByKind = map[errs.Kind]int{
	errs.KindIncorrectInput:  http.StatusBadRequest,
	errs.KindUnauthenticated: http.StatusUnauthorized,
	errs.KindForbidden:       http.StatusForbidden,
	errs.KindNotFound:        http.StatusNotFound,
	errs.KindConflict:        http.StatusConflict,
	errs.KindPayloadTooLarge: http.StatusRequestEntityTooLarge,
	errs.KindTooManyRequests: http.StatusTooManyRequests,
	errs.KindUnknown:         http.StatusInternalServerError,
	errs.KindNotImplemented:  http.StatusNotImplemented,
	errs.KindUnavailable:     http.StatusServiceUnavailable,
	errs.KindTimeout:         http.StatusGatewayTimeout,
}

// StatusOf — HTTP-статус класса ошибки. Неизвестный Kind — 500: новый класс
// без строки в таблице не должен утечь клиенту чужим статусом.
func StatusOf(kind errs.Kind) int {
	if status, ok := statusByKind[kind]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// StaticBody — тело ошибки для мест, где ответ пишет чужой код
// (http.TimeoutHandler, http.MaxBytesHandler): та же форма, что у Write, но
// без request_id — контекста запроса там нет.
//
// Паникует на негодном слаге: вызывается при сборке сервера, а годный слаг
// не требует экранирования в JSON.
func StaticBody(slug string) string {
	if !errs.ValidSlug(slug) {
		panic(fmt.Sprintf("httperr.StaticBody: slug %q must be a valid slug (errs.ValidSlug)", slug))
	}
	return `{"slug":"` + slug + `"}`
}
