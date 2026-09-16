package inbox_test

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

	"github.com/nrect/rebar/inbox"
)

// Каждая константа Outcome — в AllOutcomes: исход мимо списка попал бы в метку,
// но не в заведённые нулём пары и не в алерты.
func TestAllOutcomesIsComplete(t *testing.T) {
	t.Parallel()

	var declared []string
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		declared = append(declared, outcomeConsts(f)...)
	}
	listed := make([]string, 0, len(inbox.AllOutcomes))
	for _, outcome := range inbox.AllOutcomes {
		listed = append(listed, string(outcome))
	}
	require.NotEmpty(t, declared, "в пакете нет констант Outcome — тест смотрит не туда")
	slices.Sort(declared)
	slices.Sort(listed)
	assert.Equal(t, declared, listed)
}

// Исходы годятся меткой: непустые, уникальные, нижний snake_case.
func TestOutcomesAreLabelSafe(t *testing.T) {
	t.Parallel()

	seen := map[inbox.Outcome]bool{}
	for _, outcome := range inbox.AllOutcomes {
		assert.False(t, seen[outcome], "повтор %q", outcome)
		seen[outcome] = true
		assert.NotEmpty(t, outcome)
		for _, c := range []byte(outcome) {
			assert.True(t, c >= 'a' && c <= 'z' || c == '_', "исход %q не snake_case", outcome)
		}
	}
}

func outcomeConsts(f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			if ident, isIdent := value.Type.(*ast.Ident); !isIdent || ident.Name != "Outcome" {
				continue
			}
			for _, v := range value.Values {
				if lit, isLit := v.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
					if unquoted, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, unquoted)
					}
				}
			}
		}
	}
	return out
}
