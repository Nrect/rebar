package errstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

// Корпус нарушений: каждая находка названа файлом, строкой и причиной. Обёртка
// %w, неэкспортируемая, не-ошибка, локальная, _test.go и вложенный testdata —
// не находки, и список это тоже утверждает.
func TestEveryErrorHasKind_FindsEverySentinelWithoutKind(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everyErrorHasKind(rec, filepath.Join("testdata", "kinds", "bad"), nil)

	assert.Empty(t, rec.fatals)
	assert.Equal(t, []string{
		"aliased.go:11: ErrAliasedPlain — " + msgErrorsNew,
		"aliased.go:12: ErrAliasedUnknown — " + msgKindUnknown,
		"sentinels.go:11: ErrPlain — " + msgErrorsNew,
		"sentinels.go:14: ErrFormatted — " + msgErrorfNoWrap,
		"sentinels.go:15: ErrPercent — " + msgErrorfNoWrap,
		"sentinels.go:16: ErrFormatVar — " + msgUnknownForm,
		"sentinels.go:17: ErrUnknown — " + msgKindUnknown,
		"sentinels.go:18: ErrNewUnknown — " + msgKindUnknown,
		"sentinels.go:19: ErrSlugless — " + msgUnknownCtor,
		"sentinels.go:20: ErrHandBuilt — " + msgKindErrorLit,
		"sentinels.go:21: ErrZero — " + msgNoValue,
		"sentinels.go:22: ErrNoValue — " + msgNoValue,
		"sentinels.go:23: ErrOpaque — " + msgUnknownForm,
		"sentinels.go:26: ErrFirst — " + msgErrorsNew,
		"sentinels.go:28: ErrPairA — " + msgUnknownForm,
		"sentinels.go:28: ErrPairB — " + msgUnknownForm,
		"sentinels.go:31: NotNamedErr — " + msgErrorsNew,
		"sub/sub.go:6: ErrInSubpackage — " + msgErrorsNew,
	}, rec.errors)
}

// Отказ от класса — директива с доводом над объявлением. Без довода и при
// классе — свои находки, отличные от «без класса»; директива над блоком
// var ( … ) и с пробелом после // не действует.
func TestEveryErrorHasKind_NoKindDirective(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everyErrorHasKind(rec, filepath.Join("testdata", "kinds", "nokind"), nil)

	assert.Empty(t, rec.fatals)
	assert.Equal(t, []string{
		"nokind.go:16: ErrNoReason — " + msgNoKindNoReason,
		"nokind.go:19: ErrBothWays — " + msgNoKindHasKind,
		"nokind.go:27: ErrWrappedRefused — " + msgNoKindHasKind,
		"nokind.go:32: ErrUnderBlockDirective — " + msgErrorsNew,
		"nokind.go:36: ErrSpaced — " + msgErrorsNew,
	}, rec.errors)
}

func TestEveryErrorHasKind_CleanPackagePasses(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everyErrorHasKind(rec, filepath.Join("testdata", "kinds", "good"), nil)

	assert.Empty(t, rec.errors)
	assert.Empty(t, rec.fatals)
}

// Страж обходит исходники, а не список: новый файл с sentinel без класса
// находится тем же вызовом, без правки теста.
func TestEveryErrorHasKind_NewFileIsCaughtWithoutEditingTheTest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	check := func() []string {
		rec := &recorder{}
		everyErrorHasKind(rec, dir, nil)
		assert.Empty(t, rec.fatals)
		return rec.errors
	}

	writeSource(t, dir, "early.go",
		"package late\n\nimport \""+errsImportPath+"\"\n\nvar ErrEarly = errs.Kinded(errs.KindConflict, \"late: early\")\n")
	require.Empty(t, check(), "sentinel с классом — не находка")

	writeSource(t, dir, "late.go",
		"package late\n\nimport \"errors\"\n\nvar ErrLate = errors.New(\"late: forgotten\")\n")
	assert.Equal(t, []string{"late.go:5: ErrLate — " + msgErrorsNew}, check())
}

// allow снимает проверку с файлов по пути; подпакет остаётся под стражем.
func TestEveryErrorHasKind_Allow(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everyErrorHasKind(rec, filepath.Join("testdata", "kinds", "bad"), []string{"aliased.go", "sentinels.go"})

	assert.Equal(t, []string{"sub/sub.go:6: ErrInSubpackage — " + msgErrorsNew}, rec.errors)
}

func TestEveryErrorHasKind_UnparsableFileIsFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSource(t, dir, "broken.go", "package broken\n\nvar ErrBroken = errors.New(\n")

	rec := &recorder{}
	everyErrorHasKind(rec, dir, nil)

	assert.Empty(t, rec.errors)
	assert.Len(t, rec.fatals, 1)
}

// constructorKind узнаёт конструкторы errs по имени класса, без таблицы. Правило
// имени сторожится здесь: конструктор, выпавший из него, стал бы «не определить».
func TestConstructorKind_KnowsEveryErrsConstructor(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "constructors.go"), nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	covered := make(map[errs.Kind]bool, len(errs.AllKinds))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() {
			continue
		}
		kind, known := constructorKind(fn.Name.Name)
		require.Truef(t, known, "конструктор errs.%s не узнан по имени класса", fn.Name.Name)
		covered[kind] = true
	}
	assert.Len(t, covered, len(errs.AllKinds), "у каждого класса есть конструктор, узнанный по имени")
}

func writeSource(t *testing.T, dir, name, src string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
}
