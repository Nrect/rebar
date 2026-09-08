package authz_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
)

// ЗАКРЫТЫЙ НАБОР ПРИЧИН — МЕТКА МЕТРИКИ. Константа, добавленная в код и
// забытая в AllReasons, попала бы на дашборд мимо всякой проверки, поэтому
// список сверяется с исходником, а не с самим собой.
func TestAllReasons_MatchesDeclaredConstants(t *testing.T) {
	t.Parallel()

	declared := reasonConstants(t)
	listed := make(map[string]bool, len(authz.AllReasons))
	for _, r := range authz.AllReasons {
		assert.NotEmpty(t, string(r), "пустая причина не годится меткой метрики")
		assert.False(t, listed[string(r)], "причина %q в AllReasons дважды", r)
		listed[string(r)] = true
	}

	assert.Len(t, authz.AllReasons, len(declared))
	for _, value := range declared {
		assert.True(t, listed[value], "константа Reason %q не попала в AllReasons", value)
	}
}

// В метку метрики не должно попасть ничего, кроме закрытого набора: причины
// именуются так, чтобы отказ по правилу отличался от сбоя.
func TestAllReasons_NamingIsStable(t *testing.T) {
	t.Parallel()

	for _, r := range authz.AllReasons {
		if r == authz.ReasonAllow || r == authz.ReasonError {
			continue
		}
		assert.True(t, strings.HasPrefix(string(r), "deny_"), "отказ %q обязан читаться как отказ", r)
	}
}

// reasonConstants — значения всех констант типа Reason из исходника пакета.
func reasonConstants(t *testing.T) []string {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	require.NoError(t, err)

	var out []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := reasonValue(spec)
			if ok {
				out = append(out, value)
			}
		}
	}
	require.NotEmpty(t, out, "в types.go не найдено ни одной константы Reason")
	return out
}

// reasonValue — литерал константы, если она объявлена с типом Reason.
func reasonValue(spec ast.Spec) (string, bool) {
	value, ok := spec.(*ast.ValueSpec)
	if !ok || len(value.Values) != 1 {
		return "", false
	}
	ident, ok := value.Type.(*ast.Ident)
	if !ok || ident.Name != "Reason" {
		return "", false
	}
	lit, ok := value.Values[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, `"`), true
}

// Нулевой субъект — аноним, и это отказ: отсутствие аутентификации не должно
// выглядеть как успешная проверка прав.
func TestSubject_Anonymous(t *testing.T) {
	t.Parallel()

	assert.True(t, authz.Subject{}.Anonymous())
	assert.True(t, authz.Subject{Realm: "staff"}.Anonymous(), "реалм без идентификатора — всё ещё аноним")
	assert.False(t, subject("v").Anonymous())
}

// Нулевой ресурс — «не назван»: хук политики отличает его от названного.
func TestResource_Zero(t *testing.T) {
	t.Parallel()

	assert.True(t, authz.Resource{}.Zero())
	assert.False(t, authz.Resource{Type: "order"}.Zero())
	assert.False(t, authz.Resource{ID: "42"}.Zero())
}
