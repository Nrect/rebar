package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
)

// Разрешения и роли витрины: три разрешения, три роли цепочкой
// manager → clerk → viewer.
const (
	permRead   authz.Permission = "order.read"
	permWrite  authz.Permission = "order.write"
	permStaff  authz.Permission = "staff.manage"
	roleViewer authz.Role       = "viewer"
	roleClerk  authz.Role       = "clerk"
	roleAdmin  authz.Role       = "manager"
)

// opRead, opPublic, opUnknown — операции API потребителя.
const (
	opRead    authz.Operation = "GET /orders"
	opPublic  authz.Operation = "GET /health"
	opUnknown authz.Operation = "POST /orders/import"
)

func validConfig() authz.Config {
	return authz.Config{
		Permissions: []authz.Permission{permRead, permWrite, permStaff},
		Roles: map[authz.Role][]authz.Permission{
			roleViewer: {permRead},
			roleClerk:  {permWrite},
			roleAdmin:  {permStaff},
		},
		Inherits: map[authz.Role][]authz.Role{
			roleClerk: {roleViewer},
			roleAdmin: {roleClerk},
		},
	}
}

func validRules() map[authz.Operation]authz.Rule {
	return map[authz.Operation]authz.Rule{
		opRead:   {Permission: permRead},
		opPublic: {Public: true, Why: "проба живости для балансировщика: субъекта у неё нет"},
	}
}

// subject — субъект витрины.
func subject(id string) authz.Subject { return authz.Subject{Realm: "staff", ID: id} }

// newAuthorizer — авторизатор на двойнике источника ролей.
func newAuthorizer(t *testing.T, policy authz.Policy) (*authz.Authorizer, *authztest.MemRoles) {
	t.Helper()
	src := authztest.NewMemRoles()
	reg := authz.NewRegistry(validConfig(), validRules())
	return authz.New(src, validConfig(), policy, reg), src
}

// requirePanics — конструктор обязан отвергнуть негодное на старте, и текст
// паники обязан называть поле.
func requirePanics(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "негодная конфигурация обязана быть отвергнута")
		text, ok := r.(string)
		require.True(t, ok, "паника обязана быть строкой")
		assert.Contains(t, text, want)
	}()
	fn()
}

// decide — решение без ошибки: подготовка данных теста.
func decide(t *testing.T, a *authz.Authorizer, s authz.Subject, p authz.Permission) authz.Decision {
	t.Helper()
	d, err := a.Can(t.Context(), s, p)
	require.NoError(t, err)
	return d
}
