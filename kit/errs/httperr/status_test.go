package httperr_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// Таблица «класс → статус» поимённо: статусы попадают в алерты потребителя,
// и сдвиг любого из них обязан быть виден в диффе теста.
func TestStatusOf(t *testing.T) {
	t.Parallel()

	for kind, want := range map[errs.Kind]int{
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
	} {
		assert.Equalf(t, want, httperr.StatusOf(kind), "StatusOf(%q)", kind)
	}
}

// Unavailable — 503, а не 502: клиенты и провайдеры повторяют по нему.
func TestStatusOf_UnavailableIsNotBadGateway(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, http.StatusBadGateway, httperr.StatusOf(errs.KindUnavailable))
}

// Неизвестный класс — 500, а не паника и не чужой статус.
func TestStatusOf_UnknownKindFallsBackTo500(t *testing.T) {
	t.Parallel()

	assert.Equal(t, http.StatusInternalServerError, httperr.StatusOf(errs.Kind("выдуманный")))
}

// Тот же guard, что ставит у себя потребитель.
func TestKindStatusTable(t *testing.T) {
	t.Parallel()

	errstest.KindStatusTable(t)
}

func TestStaticBody(t *testing.T) {
	t.Parallel()

	assert.JSONEq(t, `{"slug":"internal-server-error"}`, httperr.StaticBody(httperr.DefaultInternalSlug))

	body := httperr.StaticBody("request-timeout")
	assert.JSONEq(t, `{"slug":"request-timeout"}`, body)
	assert.NotContains(t, body, " ", "тело уходит в http.TimeoutHandler как есть, без лишних байт")
	assert.PanicsWithValue(t,
		`httperr.StaticBody: slug "Bad Slug\"" must be a valid slug (errs.ValidSlug)`,
		func() { httperr.StaticBody(`Bad Slug"`) })
}
