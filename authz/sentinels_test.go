package authz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: своего класса у их sentinel нет — класс
// приходит обёрткой (ADR-0007, «Двойники»).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "authztest")
}

type sentinel struct {
	name   string
	err    error
	kind   errs.Kind
	prefix string
}

func sentinels() []sentinel {
	return []sentinel{
		{"authz.ErrUnavailable", authz.ErrUnavailable, errs.KindUnavailable, "authz: "},
		{"authz.ErrUnknownPermission", authz.ErrUnknownPermission, errs.KindUnknown, "authz: "},
		{"authz.ErrDenied", authz.ErrDenied, errs.KindForbidden, "authz: "},
		{"authzpg.ErrInvalidAssignment", authzpg.ErrInvalidAssignment, errs.KindUnknown, "authzpg: "},
	}
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через
// errors.Is: у authz.ErrUnavailable и entitlement.ErrUnavailable текст после
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

// Сбой источника ролей и хука политики доходит до вызывающего с классом 503:
// класс несёт обёртка ядра. Источник ролей здесь — голая заглушка:
// authztest.MemRoles заворачивает сбой сам, как authzpg, и снятой обёртки ядра
// страж бы не увидел. Хук политики пишет потребитель — он отдаёт сбой голым.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")

	reg := authz.NewRegistry(validConfig(), validRules())
	brokenSource := authz.New(bareRoles{err: down}, validConfig(), nil, reg)
	brokenPolicy, roles := newAuthorizer(t, func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		return false, down
	})
	roles.Set(subject("v"), roleViewer)

	for port, a := range map[string]*authz.Authorizer{"источник ролей": brokenSource, "хук политики": brokenPolicy} {
		_, err := a.Can(t.Context(), subject("v"), permRead)
		assertUnavailable(t, err, down, port+": Can")
		_, err = a.CanOn(t.Context(), subject("v"), permRead, authz.Resource{Type: "order", ID: "42"})
		assertUnavailable(t, err, down, port+": CanOn")
		_, err = a.CanOp(t.Context(), subject("v"), opRead)
		assertUnavailable(t, err, down, port+": CanOp")
		assertUnavailable(t, a.Require(t.Context(), subject("v"), permRead, authz.Resource{}), down, port+": Require")
	}
}

// assertUnavailable — класс 503 и причина в цепочке: обёртка не потеряла ни
// класса, ни исходной ошибки.
func assertUnavailable(t *testing.T, err, cause error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	assert.ErrorIsf(t, err, cause, "причина на %s", site)
}

// bareRoles — authz.RoleSource, отдающий сбой голым.
type bareRoles struct{ err error }

func (r bareRoles) RolesOf(context.Context, authz.Subject) ([]authz.Role, error) { return nil, r.err }
