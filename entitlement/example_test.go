package entitlement_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementtest"
)

// Проводка потребителя: порт двойником, решение с причиной, отказ и
// недоступность как разные ошибки.
func Example() {
	store := entitlementtest.NewMemStore()
	svc := entitlement.New(store, entitlement.Config{
		TTL:         5 * time.Minute,
		MaxSubjects: 10_000,
		LoadTimeout: 3 * time.Second,
	})

	ctx := context.Background()
	subject := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	until := time.Now().Add(24 * time.Hour)
	if err := svc.Grant(ctx, subject, entitlement.Grant{ItemID: "course.algebra", ExpiresAt: &until}); err != nil {
		panic(err)
	}

	bought, _ := svc.Allows(ctx, subject, "course.algebra")
	fmt.Println(bought.Allowed, bought.Reason)

	other, _ := svc.Allows(ctx, subject, "course.geometry")
	fmt.Println(other.Allowed, other.Reason)

	// В хендлере решение удобнее ошибкой: 403 и 503 — разные инциденты.
	err := svc.Require(ctx, subject, "course.geometry")
	fmt.Println(errors.Is(err, entitlement.ErrDenied), errors.Is(err, entitlement.ErrUnavailable))

	// Output:
	// true allow
	// false deny_no_grant
	// true false
}
