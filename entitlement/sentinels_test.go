package entitlement_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: их ошибки — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "entitlementtest")
}

type sentinel struct {
	name   string
	err    error
	kind   errs.Kind
	prefix string
}

func sentinels() []sentinel {
	return []sentinel{
		{"entitlement.ErrUnavailable", entitlement.ErrUnavailable, errs.KindUnavailable, "entitlement: "},
		{"entitlement.ErrDenied", entitlement.ErrDenied, errs.KindForbidden, "entitlement: "},
		{"entitlement.ErrInvalidGrant", entitlement.ErrInvalidGrant, errs.KindUnknown, "entitlement: "},
	}
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через
// errors.Is: у entitlement.ErrUnavailable и authz.ErrUnavailable текст после
// префикса один.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range sentinels() {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), tc.prefix), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Внутри модуля префикс не различает: две sentinel одного пакета с одним
// классом и текстом были бы взаимозаменяемы через errors.Is.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	all := sentinels()
	for i, a := range all {
		for j, b := range all {
			if i != j {
				assert.NotErrorIsf(t, a.err, b.err, "%s совпала с %s через errors.Is", a.name, b.name)
			}
		}
	}
}

// Сбой хранилища на путях чтения и записи доходит до вызывающего с классом 503,
// а не голой причиной двойника: класс несёт обёртка ядра.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	svc, store, _ := newService(t)
	store.SetErr(errStore)
	subject := uuid.New()

	_, err := svc.Allows(t.Context(), subject, itemAlgebra)
	assertUnavailable(t, err, "Allows")
	assertUnavailable(t, svc.Require(t.Context(), subject, itemAlgebra), "Require")
	_, err = svc.Open(t.Context(), subject)
	assertUnavailable(t, err, "Open")
	assertUnavailable(t, svc.Grant(t.Context(), subject, entitlement.Grant{ItemID: itemAlgebra}), "Grant")
	assertUnavailable(t, svc.Revoke(t.Context(), subject, itemAlgebra), "Revoke")
}

// assertUnavailable — класс 503 и причина двойника в цепочке: обёртка не
// потеряла ни класса, ни исходной ошибки.
func assertUnavailable(t *testing.T, err error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	assert.ErrorIsf(t, err, errStore, "причина двойника на %s", site)
}
