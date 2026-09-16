package ledger_test

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

	"github.com/nrect/rebar/ledger"
)

// closedSets — закрытые наборы пакета и их списки All*: значения уходят в
// справочники хранилища и в метки метрик сверки.
var closedSets = map[string][]string{
	"Sign":        strs(ledger.AllSigns),
	"Requirement": strs(ledger.AllRequirements),
	"Check":       strs(ledger.AllChecks),
}

// Каждая объявленная константа закрытого типа — в своём All*. Значение мимо
// списка попало бы в базу и в метку, но не в CHECK схемы и не в алерты.
func TestClosedSetsAreComplete(t *testing.T) {
	t.Parallel()

	declared := map[string][]string{}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		collectConsts(f, declared)
	}
	for typeName, listed := range closedSets {
		found := declared[typeName]
		require.NotEmpty(t, found, "в пакете нет ни одной константы типа %s — тест смотрит не туда", typeName)
		slices.Sort(found)
		assert.Equal(t, found, listed, "список All* и константы типа %s разъехались", typeName)
	}
}

// Значения годятся колонкой базы и меткой: непустые, уникальные, нижний
// snake_case.
func TestClosedSetValuesAreLabelSafe(t *testing.T) {
	t.Parallel()

	for typeName, values := range closedSets {
		assert.Len(t, values, len(slices.Compact(slices.Clone(values))), "повтор в %s", typeName)
		for _, v := range values {
			assert.NotEmpty(t, v, "пустое значение в %s", typeName)
			for i := range len(v) {
				c := v[i]
				assert.True(t, c >= 'a' && c <= 'z' || c == '_', "значение %q типа %s не snake_case", v, typeName)
			}
		}
	}
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
			if !ok || closedSets[ident.Name] == nil {
				continue
			}
			for _, v := range value.Values {
				if lit, isLit := v.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
					unquoted, err := strconv.Unquote(lit.Value)
					if err == nil {
						out[ident.Name] = append(out[ident.Name], unquoted)
					}
				}
			}
		}
	}
}

// strs — типизированный список как отсортированные строки.
func strs[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	slices.Sort(out)
	return out
}
