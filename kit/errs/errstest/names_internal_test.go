package errstest

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noPrefix — находка «текст начинается не с <pkg>: » для пакета pkg.
func noPrefix(pkg string) string { return fmt.Sprintf(msgNoPackagePrefix, pkg+": ") }

// Корпус нарушений: каждая находка названа файлом, строкой и причиной. Обёртка
// %w, конструкторы SlugError, неузнанная форма, неэкспортируемая, локальная,
// _test.go и вложенный testdata — не находки, и список это тоже утверждает.
func TestEverySentinelNamesItsPackage_FindsEverySentinelWithoutPrefix(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everySentinelNamesItsPackage(rec, filepath.Join("testdata", "names", "bad"), nil)

	assert.Empty(t, rec.fatals)
	assert.Equal(t, []string{
		"aliased.go:11: ErrAliasedPlain — " + noPrefix("bad"),
		"aliased.go:12: ErrAliasedKinded — " + noPrefix("bad"),
		"sentinels.go:11: ErrNoPrefix — " + noPrefix("bad"),
		"sentinels.go:14: ErrOtherPackage — " + noPrefix("bad"),
		"sentinels.go:15: ErrNoSpace — " + noPrefix("bad"),
		"sentinels.go:16: ErrNotAtStart — " + noPrefix("bad"),
		"sentinels.go:17: ErrFormatted — " + noPrefix("bad"),
		"sentinels.go:18: ErrRaw — " + noPrefix("bad"),
		"sentinels.go:19: ErrKindedConst — " + msgTextNotLiteral,
		"sentinels.go:20: ErrNewConst — " + msgTextNotLiteral,
		"sentinels.go:23: ErrFirst — " + noPrefix("bad"),
		"sentinels.go:26: NotNamedErr — " + noPrefix("bad"),
		"sentinels.go:32: ErrRefused — " + noPrefix("bad"),
		"sub/sub.go:8: ErrInSubpackage — " + noPrefix("sub"),
	}, rec.errors)
}

// Префикс — имя ПАКЕТА, а не корня обхода: в подпакете годен свой, и текст с
// префиксом корня там находка.
func TestEverySentinelNamesItsPackage_PrefixIsPerPackage(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everySentinelNamesItsPackage(rec, filepath.Join("testdata", "names", "bad", "sub"), nil)

	assert.Empty(t, rec.fatals)
	assert.Equal(t, []string{"sub.go:8: ErrInSubpackage — " + noPrefix("sub")}, rec.errors)
}

func TestEverySentinelNamesItsPackage_CleanPackagePasses(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everySentinelNamesItsPackage(rec, filepath.Join("testdata", "names", "good"), nil)

	assert.Empty(t, rec.errors)
	assert.Empty(t, rec.fatals)
}

// Страж обходит исходники, а не список: новая sentinel без префикса находится
// тем же вызовом, без правки теста. Рукописная проверка в модуле этого не
// умела — она перебирала таблицу, и забытая в ней sentinel не проверялась.
func TestEverySentinelNamesItsPackage_NewFileIsCaughtWithoutEditingTheTest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	check := func() []string {
		rec := &recorder{}
		everySentinelNamesItsPackage(rec, dir, nil)
		assert.Empty(t, rec.fatals)
		return rec.errors
	}

	writeSource(t, dir, "early.go",
		"package late\n\nimport \""+errsImportPath+"\"\n\nvar ErrEarly = errs.Kinded(errs.KindConflict, \"late: early\")\n")
	require.Empty(t, check(), "sentinel с префиксом — не находка")

	writeSource(t, dir, "late.go",
		"package late\n\nimport \"errors\"\n\nvar ErrLate = errors.New(\"forgotten\")\n")
	assert.Equal(t, []string{"late.go:5: ErrLate — " + noPrefix("late")}, check())
}

// allow снимает проверку с файлов по пути; подпакет остаётся под стражем.
func TestEverySentinelNamesItsPackage_Allow(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	everySentinelNamesItsPackage(rec, filepath.Join("testdata", "names", "bad"), []string{"aliased.go", "sentinels.go"})

	assert.Equal(t, []string{"sub/sub.go:8: ErrInSubpackage — " + noPrefix("sub")}, rec.errors)
}

func TestEverySentinelNamesItsPackage_UnparsableFileIsFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSource(t, dir, "broken.go", "package broken\n\nvar ErrBroken = errors.New(\n")

	rec := &recorder{}
	everySentinelNamesItsPackage(rec, dir, nil)

	assert.Empty(t, rec.errors)
	assert.Len(t, rec.fatals, 1)
}
