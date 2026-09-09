package entitlement_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// allowedByDir — БЕЛЫЙ список внешних импортов по каталогу. Белый, а не
// чёрный: чёрный пропускает всё, о чём не подумали, и выключается целиком
// после переезда каталога в другой модуль. Новый подпакет добавляется сюда
// тем же коммитом, что и каталог.
//
// uuid — единственная внешняя зависимость ядра: идентификатор субъекта стоит
// в сигнатуре порта, и своего типа для него пакет не заводит (ADR-0005).
// Кэш и объединение одинаковых запросов написаны на stdlib, поэтому строки
// про библиотеку кэша здесь нет и появиться она может только с причиной.
var allowedByDir = map[string][]string{
	".":               {"github.com/google/uuid"},
	"entitlementtest": {"github.com/google/uuid"},
}

// TestPackageImportsAreWhitelisted — страж переносимости: ни одного импорта
// из модуля-потребителя, у каждого каталога свой белый список внешних
// зависимостей плюс stdlib. Список белый, а не чёрный, и путь модуля читается
// из go.mod, а не из литерала — иначе страж выключается после копирования
// каталога в чужой проект. _test.go не проверяются.
func TestPackageImportsAreWhitelisted(t *testing.T) {
	t.Parallel()

	if found := scanImports(t, ".", allowedByDir); len(found) != 0 {
		t.Errorf("страж импортов нашёл нарушения:\n%s", strings.Join(found, "\n"))
	}
}

// СТРАЖ, КОТОРЫЙ МОЛЧИТ ВСЕГДА, ВЫГЛЯДИТ КАК РАБОТАЮЩИЙ. Проверка на
// срабатывание: корпус в testdata с заведомо запрещённым импортом обязан дать
// ровно одну находку (PATTERNS 10).
func TestImportGuardFires(t *testing.T) {
	t.Parallel()

	found := scanImports(t, "testdata/badcore", map[string][]string{".": {}})
	if len(found) != 2 {
		t.Fatalf("на корпусе с двумя запрещёнными импортами ожидалось две находки, получено %d: %v", len(found), found)
	}
	all := strings.Join(found, "\n")
	// Второй — соседний модуль тулкита: он не stdlib и не «свой», и списком
	// каталога не разрешён, значит обязан попасть в находки наравне с чужой
	// библиотекой.
	for _, want := range []string{"github.com/jackc/pgx/v5", "github.com/nrect/rebar/authz"} {
		if !strings.Contains(all, want) {
			t.Errorf("находки не называют запрещённый импорт %s: %s", want, all)
		}
	}
}

// scanImports — импорты всех .go под root, сверенные с белым списком по
// каталогу (ключ — путь относительно root). Каталог testdata пропускается:
// он корпус стража, а не код пакета.
func scanImports(t *testing.T, root string, allowedByDir map[string][]string) []string {
	t.Helper()

	module, selfImport := selfImportPath(t)
	fset := token.NewFileSet()
	var found []string

	walkErr := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" && name != root {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir, relErr := filepath.Rel(root, filepath.Dir(name))
		if relErr != nil {
			return relErr
		}
		dir = filepath.ToSlash(dir)
		allowed, known := allowedByDir[dir]
		if !known {
			found = append(found, name+": каталог "+dir+" не описан в allowedByDir — новый подпакет обязан объявить свой белый список")
			return nil
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if allowedImport(importPath, module, selfImport, allowed) {
				continue
			}
			found = append(found, name+": импортирует "+importPath+" — каталогу "+dir+" положены только stdlib и "+strings.Join(allowed, ", "))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	return found
}

// allowedImport — stdlib узнаётся по первому сегменту без точки; собственный
// модуль проверяется до этого правила, потому что его путь тоже может быть
// без точки.
func allowedImport(importPath, module, selfImport string, allowed []string) bool {
	switch {
	case importPath == selfImport, strings.HasPrefix(importPath, selfImport+"/"):
		return true
	case importPath == module, strings.HasPrefix(importPath, module+"/"):
		return false
	}
	for _, prefix := range allowed {
		if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
			return true
		}
	}
	segment, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(segment, ".")
}

// selfImportPath — путь модуля и импорт-путь пакета из go.mod и положения
// каталога. Импорт-путь один на весь обход: подпакеты вправе импортировать
// ядро, а вот модуль-потребитель — нет.
func selfImportPath(t *testing.T) (module, selfImport string) {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for root := dir; ; {
		goMod, readErr := os.ReadFile(filepath.Join(root, "go.mod"))
		if readErr == nil {
			rel, relErr := filepath.Rel(root, dir)
			if relErr != nil {
				t.Fatal(relErr)
			}
			module = modulePath(t, string(goMod))
			return module, path.Join(module, filepath.ToSlash(rel))
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("go.mod не найден ни в одном каталоге выше %s", dir)
		}
		root = parent
	}
}

func modulePath(t *testing.T, goMod string) string {
	t.Helper()
	for line := range strings.SplitSeq(goMod, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("в go.mod нет строки module")
	return ""
}
