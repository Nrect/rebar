package audit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
)

// closedSets — закрытые наборы пакета и их списки All*.
//
// Action сюда НЕ входит намеренно: его набор закрывает потребитель
// (Config.Actions), пакет проверяет только форму — как Kind в mail. Страж
// реестра действий — TestActionRegistry_IsClosed.
var closedSets = map[string][]string{
	"Outcome":   strs(audit.AllOutcomes),
	"ActorKind": strs(audit.AllActorKinds),
}

// TestClosedSetsAreComplete — страж закрытых наборов: КАЖДАЯ объявленная в
// пакете константа этих типов обязана быть в списке All*.
//
// Проверяется по исходникам, а не длиной списка, потому что дефект здесь
// молчаливый: значение мимо списка попадает в колонку и в метку метрики, но
// не попадает ни в CHECK схемы, ни в документацию алертов.
func TestClosedSetsAreComplete(t *testing.T) {
	t.Parallel()

	declared := map[string][]string{}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		collectConsts(f, declared)
	}

	for typeName, listed := range closedSets {
		found := declared[typeName]
		require.NotEmpty(t, found, "в пакете нет ни одной константы типа %s — тест смотрит не туда", typeName)
		for _, value := range found {
			assert.Contains(t, listed, value, "значение %q типа %s объявлено мимо списка All*", value, typeName)
		}
		assert.Len(t, listed, len(found), "список All* и константы типа %s разъехались", typeName)
	}
}

// TestClosedSetValuesAreLabelSafe — значения годятся меткой метрики и колонкой
// БД: непустые, уникальные, в нижнем snake_case. Разъедься регистр — один
// исход стал бы двумя рядами на дашборде.
func TestClosedSetValuesAreLabelSafe(t *testing.T) {
	t.Parallel()

	for typeName, values := range closedSets {
		seen := map[string]bool{}
		for _, v := range values {
			assert.NotEmpty(t, v, "пустое значение в %s", typeName)
			assert.False(t, seen[v], "значение %q в %s перечислено дважды", v, typeName)
			seen[v] = true
			for i := range len(v) {
				c := v[i]
				assert.True(t, c >= 'a' && c <= 'z' || c == '_', "значение %q типа %s не snake_case", v, typeName)
			}
		}
	}
}

// Отказ и сбой — разные исходы, и сводить их нельзя: всплеск denied это подбор
// пароля, всплеск failure — авария, и алерты у них разные.
func TestOutcomes_DeniedAndFailureAreDistinct(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, audit.OutcomeDenied, audit.OutcomeFailure)
	assert.Contains(t, strs(audit.AllOutcomes), "denied")
	assert.Contains(t, strs(audit.AllOutcomes), "failure")
}

// Аноним — род актора, а не отсутствие актора: обвязка обязана назвать его
// явно, иначе Prepare отвечает ErrNoActor (doc.go, п. 3).
func TestActorKinds_AnonymousIsAKind(t *testing.T) {
	t.Parallel()

	assert.Contains(t, strs(audit.AllActorKinds), "anonymous")
}

func collectConsts(f *ast.File, out map[string][]string) {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok {
				continue
			}
			if _, known := closedSets[ident.Name]; !known {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out[ident.Name] = append(out[ident.Name], unquoted)
			}
		}
	}
}

// strs — типизированный список значений как строки.
func strs[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	slices.Sort(out)
	return out
}
