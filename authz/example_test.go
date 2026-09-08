package authz_test

import (
	"context"
	"fmt"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
)

// Проводка целиком: модель прав, реестр операций API, источник ролей.
// Операция без правила отказывает, публичная — разрешена анониму.
func Example() {
	cfg := authz.Config{
		Permissions: []authz.Permission{"order.read", "order.write"},
		Roles: map[authz.Role][]authz.Permission{
			"viewer": {"order.read"},
			"clerk":  {"order.write"},
		},
		Inherits: map[authz.Role][]authz.Role{"clerk": {"viewer"}},
	}
	reg := authz.NewRegistry(cfg, map[authz.Operation]authz.Rule{
		"GET /orders":  {Permission: "order.read"},
		"POST /orders": {Permission: "order.write"},
		"GET /health":  {Public: true, Why: "проба живости балансировщика: субъекта у неё нет"},
	})

	roles := authztest.NewMemRoles() // в проде — authzpg.Store или свой адаптер
	clerk := authz.Subject{Realm: "staff", ID: "u1"}
	roles.Set(clerk, "clerk")

	a := authz.New(roles, cfg, nil, reg)
	ctx := context.Background()

	for _, op := range []authz.Operation{"GET /orders", "GET /health", "DELETE /orders/1"} {
		d, err := a.CanOp(ctx, clerk, op)
		fmt.Printf("%-16s allowed=%-5v reason=%s err=%v\n", op, d.Allowed, d.Reason, err)
	}

	anon, _ := a.CanOp(ctx, authz.Subject{}, "GET /health")
	fmt.Printf("%-16s allowed=%-5v reason=%s\n", "аноним /health", anon.Allowed, anon.Reason)

	// Output:
	// GET /orders      allowed=true  reason=allow err=<nil>
	// GET /health      allowed=true  reason=allow err=<nil>
	// DELETE /orders/1 allowed=false reason=deny_unclassified err=<nil>
	// аноним /health   allowed=true  reason=allow
}
