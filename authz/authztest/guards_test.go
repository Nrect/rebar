package authztest_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
)

// Операции API витрины: две классифицированы, третья забыта.
const (
	opList   authz.Operation = "GET /orders"
	opHealth authz.Operation = "GET /health"
	opImport authz.Operation = "POST /orders/import"
)

func registry(t *testing.T, rules map[authz.Operation]authz.Rule) *authz.Registry {
	t.Helper()
	cfg := authz.Config{
		Permissions: []authz.Permission{"order.read"},
		Roles:       map[authz.Role][]authz.Permission{"viewer": {"order.read"}},
	}
	return authz.NewRegistry(cfg, rules)
}

// СТРАЖ ПРОВЕРЯЕТСЯ ПО ТОМУ, ЧТО ОН ПАДАЕТ. Тест «на молчание» ничего не
// доказывает: молчащий страж выглядит точно так же, как работающий
// (PATTERNS 10).
func TestRequireAllClassified(t *testing.T) {
	t.Parallel()

	reg := registry(t, map[authz.Operation]authz.Rule{
		opList:   {Permission: "order.read"},
		opHealth: {Public: true, Why: "проба живости балансировщика"},
	})
	ops := []authz.Operation{opList, opHealth}

	quiet := runGuard(authztest.RequireAllClassified, ops, reg)
	assert.False(t, quiet.failed, "все операции классифицированы, падать не на чем")

	forgotten := []authz.Operation{opList, opHealth, opImport}
	loud := runGuard(authztest.RequireAllClassified, forgotten, reg)
	require.True(t, loud.failed, "неклассифицированная операция обязана ронять тест")
	assert.Contains(t, strings.Join(loud.log, "\n"), string(opImport), "сообщение обязано называть операцию")
}

// Мёртвое правило врёт читателю о поверхности API и переживает удаление
// хендлера: следующий одноимённый получит чужие права молча.
func TestRequireNoDeadRules(t *testing.T) {
	t.Parallel()

	reg := registry(t, map[authz.Operation]authz.Rule{
		opList:   {Permission: "order.read"},
		opImport: {Permission: "order.read"},
	})

	quiet := runGuard(authztest.RequireNoDeadRules, []authz.Operation{opList, opImport}, reg)
	assert.False(t, quiet.failed)

	loud := runGuard(authztest.RequireNoDeadRules, []authz.Operation{opList}, reg)
	require.True(t, loud.failed, "мёртвое правило обязано ронять тест")
	assert.Contains(t, strings.Join(loud.log, "\n"), string(opImport))
}

// Несобранный реестр — не «пусто, значит всё классифицировано».
func TestGuards_NilRegistry(t *testing.T) {
	t.Parallel()

	ops := []authz.Operation{opList}
	assert.True(t, runGuard(authztest.RequireAllClassified, ops, nil).failed)
	assert.True(t, runGuard(authztest.RequireNoDeadRules, ops, nil).failed)
}

// fakeTB — подставной testing.TB: страж проверяется по тому, упал ли он, и с
// каким сообщением. Встроенный интерфейс нулевой — вызов не переопределённого
// метода даст панику, и это правильно: страж не вправе звать ничего сверх
// Helper, Errorf и Fatal.
type fakeTB struct {
	testing.TB
	failed bool
	log    []string
}

// errFatalStop — Fatal обязан прервать выполнение, как настоящий.
var errFatalStop = errors.New("fakeTB: Fatal")

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.failed = true
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatal(args ...any) {
	f.failed = true
	f.log = append(f.log, fmt.Sprint(args...))
	panic(errFatalStop)
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.log = append(f.log, fmt.Sprintf(format, args...))
	panic(errFatalStop)
}

// guardFunc — общая форма стражей реестра.
type guardFunc func(tb testing.TB, ops []authz.Operation, reg *authz.Registry)

// runGuard — прогон стража на подставном TB.
func runGuard(guard guardFunc, ops []authz.Operation, reg *authz.Registry) (tb *fakeTB) {
	tb = &fakeTB{}
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if err, ok := r.(error); ok && errors.Is(err, errFatalStop) {
			return
		}
		panic(r)
	}()
	guard(tb, ops, reg)
	return tb
}
