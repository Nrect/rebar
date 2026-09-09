package authzhttp_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzhttp"
	"github.com/nrect/rebar/authz/authztest"
)

const (
	permRead authz.Permission = "order.read"
	permWrp  authz.Permission = "order.write"
	roleView authz.Role       = "viewer"

	opRead   authz.Operation = "GET /orders"
	opHealth authz.Operation = "GET /health"
)

var staff = authz.Subject{Realm: "staff", ID: "u1"}

func config() authz.Config {
	return authz.Config{
		Permissions: []authz.Permission{permRead, permWrp},
		Roles:       map[authz.Role][]authz.Permission{roleView: {permRead}},
	}
}

func rules() map[authz.Operation]authz.Rule {
	return map[authz.Operation]authz.Rule{
		opRead:   {Permission: permRead},
		opHealth: {Public: true, Why: "проба живости балансировщика: субъекта у неё нет"},
	}
}

// harness — собранный guard, источник ролей и счётчик вызовов хендлера.
type harness struct {
	guard  *authzhttp.Guard
	src    *authztest.MemRoles
	served *int
	// subject — кого middleware увидит в запросе; правится тестом.
	subject *authz.Subject
}

func newHarness(t *testing.T) harness {
	t.Helper()
	src := authztest.NewMemRoles()
	a := authz.New(src, config(), nil, authz.NewRegistry(config(), rules()))
	served, subject := 0, authz.Subject{}
	guard := authzhttp.New(authzhttp.Config{
		Authorizer: a,
		Subject:    func(*http.Request) authz.Subject { return subject },
		Deny:       writeDeny,
	})
	return harness{guard: guard, src: src, served: &served, subject: &subject}
}

// writeDeny — отображение исхода в статус из doc.go: его пишет потребитель,
// и здесь оно проверяется целиком.
func writeDeny(w http.ResponseWriter, _ *http.Request, d authz.Decision, err error) {
	switch {
	case errors.Is(err, authz.ErrUnavailable):
		w.WriteHeader(http.StatusServiceUnavailable)
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
	case d.Reason == authz.ReasonNoSubject:
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusForbidden)
	}
}

func (h harness) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*h.served++
		w.WriteHeader(http.StatusOK)
	})
}

func (h harness) serve(t *testing.T, mw func(http.Handler) http.Handler) int {
	t.Helper()
	rec := httptest.NewRecorder()
	mw(h.handler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders", http.NoBody))
	return rec.Code
}

// Отказ не пускает дальше: хендлер не зовётся ни при отказе по правилу, ни
// при сбое источника ролей.
func TestGuard_Require(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		subject    authz.Subject
		roles      []authz.Role
		sourceErr  error
		wantCode   int
		wantServed int
	}{
		{name: "аноним", wantCode: http.StatusUnauthorized},
		{name: "ролей нет", subject: staff, wantCode: http.StatusForbidden},
		{
			name: "роль даёт разрешение", subject: staff, roles: []authz.Role{roleView},
			wantCode: http.StatusOK, wantServed: 1,
		},
		{
			name: "источник ролей упал", subject: staff, roles: []authz.Role{roleView},
			sourceErr: errors.New("connection refused"), wantCode: http.StatusServiceUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			*h.subject = tt.subject
			h.src.Set(tt.subject, tt.roles...)
			h.src.SetErr(tt.sourceErr)

			assert.Equal(t, tt.wantCode, h.serve(t, h.guard.Require(permRead)))
			assert.Equal(t, tt.wantServed, *h.served, "хендлер за отказом зваться не должен")
		})
	}
}

// Публичная операция разрешена анониму — ради этого правило и требует Why.
func TestGuard_RequireOp(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	assert.Equal(t, http.StatusOK, h.serve(t, h.guard.RequireOp(opHealth)))
	assert.Equal(t, 1, *h.served)

	assert.Equal(t, http.StatusUnauthorized, h.serve(t, h.guard.RequireOp(opRead)))
	assert.Equal(t, 1, *h.served)
}

// ПРОВОДКА ПРОВЕРЯЕТСЯ НА СТАРТЕ: опечатка в константе роняет сборку
// маршрутов, а не превращается в 403 на каждом запросе.
func TestGuard_PanicsOnBadWiring(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	assert.PanicsWithValue(t,
		`authzhttp.Require: permission "order.delete" is not declared in Config.Permissions`,
		func() { h.guard.Require("order.delete") })
	assert.PanicsWithValue(t,
		`authzhttp.RequireOp: operation "POST /orders" has no rule in the registry`,
		func() { h.guard.RequireOp("POST /orders") })
}

// Нулевые поля Config — паника: отсутствующий Deny означал бы отказ без
// ответа, отсутствующий Subject — «все анонимы».
func TestNew_PanicsOnNilFields(t *testing.T) {
	t.Parallel()

	a := authz.New(authztest.NewMemRoles(), config(), nil, authz.NewRegistry(config(), rules()))
	subject := func(*http.Request) authz.Subject { return staff }

	tests := []struct {
		name string
		cfg  authzhttp.Config
		want string
	}{
		{name: "без авторизатора", cfg: authzhttp.Config{Subject: subject, Deny: writeDeny},
			want: "Config.Authorizer must not be nil"},
		{name: "без источника субъекта", cfg: authzhttp.Config{Authorizer: a, Deny: writeDeny},
			want: "Config.Subject must not be nil"},
		{name: "без Deny", cfg: authzhttp.Config{Authorizer: a, Subject: subject},
			want: "Config.Deny must not be nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.PanicsWithValue(t, "authzhttp.New: "+tt.want, func() { authzhttp.New(tt.cfg) })
		})
	}
}

// Субъект виден хендлеру: проверки по ресурсу маршруту не видны и живут в нём.
func TestGuard_Subject(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	*h.subject = staff

	require.Equal(t, staff, h.guard.Subject(httptest.NewRequest(http.MethodGet, "/orders", http.NoBody)))
}
