package entitlement_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
)

// ЗАКРЫТЫЙ НАБОР ПРИЧИН — МЕТКА МЕТРИКИ. Константа, добавленная в код и
// забытая в AllReasons, попала бы на дашборд мимо всякой проверки, поэтому
// список сверяется с исходником, а не с самим собой.
func TestAllReasons_MatchesDeclaredConstants(t *testing.T) {
	t.Parallel()

	declared := reasonConstants(t)
	listed := make(map[string]bool, len(entitlement.AllReasons))
	for _, r := range entitlement.AllReasons {
		assert.NotEmpty(t, string(r), "пустая причина не годится меткой метрики")
		assert.False(t, listed[string(r)], "причина %q в AllReasons дважды", r)
		listed[string(r)] = true
	}

	assert.Len(t, entitlement.AllReasons, len(declared))
	for _, value := range declared {
		assert.True(t, listed[value], "константа Reason %q не попала в AllReasons", value)
	}
}

// Причины именуются так, чтобы отказ по правилу отличался от сбоя: на этом
// стоит алерт «хранилище недоступно», который не должен гореть от штатных 403.
func TestAllReasons_NamingIsStable(t *testing.T) {
	t.Parallel()

	for _, r := range entitlement.AllReasons {
		if r == entitlement.ReasonAllow || r == entitlement.ReasonError {
			continue
		}
		assert.True(t, strings.HasPrefix(string(r), "deny_"), "отказ %q обязан читаться как отказ", r)
	}
}

// НУЛЕВОЕ ЗНАЧЕНИЕ РЕШЕНИЯ — ОТКАЗ: забытое присваивание не открывает доступ.
func TestDecision_ZeroValueIsDeny(t *testing.T) {
	t.Parallel()

	assert.False(t, entitlement.Decision{}.Allowed)
}

// Граница срока строгая и одинаковая везде: у Grant.Open, у дедлайна снимка и
// у адаптера (expires_at > now). Разъезд этой границы — лишняя наносекунда
// оплаченного срока у одной реализации и лишняя секунда у другой.
func TestGrant_OpenBoundary(t *testing.T) {
	t.Parallel()

	expires := base.Add(time.Hour)
	limited := entitlement.Grant{ItemID: itemAlgebra, ExpiresAt: &expires}
	assert.True(t, limited.Open(expires.Add(-time.Nanosecond)))
	assert.False(t, limited.Open(expires), "момент истечения уже закрыт")
	assert.False(t, limited.Open(expires.Add(time.Nanosecond)))

	forever := entitlement.Grant{ItemID: itemAlgebra}
	assert.True(t, forever.Open(expires.AddDate(100, 0, 0)), "nil — бессрочно")
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
			if value, ok := reasonValue(spec); ok {
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
