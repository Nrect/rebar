package errs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

// Набор Kind закрытый: каждая объявленная константа обязана быть в AllKinds.
// Проверка по исходнику, а не по счётчику: забытое значение молча получило бы
// 500 в httperr и новую метку в метрике.
func TestAllKindsListsEveryConstant(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "kind.go", nil, 0)
	require.NoError(t, err)

	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || !isKindSpec(value) {
				continue
			}
			declared++
			lit, ok := value.Values[0].(*ast.BasicLit)
			require.Truef(t, ok, "%s объявлен не литералом", value.Names[0].Name)
			assert.Containsf(t, errs.AllKinds, errs.Kind(strings.Trim(lit.Value, `"`)),
				"константа %s не попала в AllKinds", value.Names[0].Name)
		}
	}

	assert.NotZero(t, declared, "в kind.go не нашлось ни одной константы Kind")
	assert.Len(t, errs.AllKinds, declared, "в AllKinds есть лишнее или повторы")
}

// isKindSpec — константа объявлена с типом Kind и одним значением.
func isKindSpec(spec *ast.ValueSpec) bool {
	ident, ok := spec.Type.(*ast.Ident)
	return ok && ident.Name == "Kind" && len(spec.Names) == 1 && len(spec.Values) == 1
}

// Значения Kind уходят клиенту в виде статуса и в метку метрики: они обязаны
// быть годными слагами и не повторяться.
func TestAllKindsAreValidSlugs(t *testing.T) {
	t.Parallel()

	seen := make(map[errs.Kind]bool, len(errs.AllKinds))
	for _, kind := range errs.AllKinds {
		assert.Truef(t, errs.ValidSlug(string(kind)), "Kind %q не kebab-case", kind)
		assert.Falsef(t, seen[kind], "Kind %q повторяется в AllKinds", kind)
		seen[kind] = true
	}
}
