package objectstore_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedByDir — белый список внешних импортов по каталогу. Новый подпакет
// добавляется сюда тем же коммитом, что и каталог; пустой список означает
// «только stdlib и собственный модуль».
//
// У адаптеров списки ПУСТЫЕ, и это не недосмотр: s3 подписывает запросы сам
// (копия приёма из mail/sesv2 — ADR-0006), fs стоит на os и path/filepath,
// imgproxy считает HMAC на crypto/hmac. Ни один не тянет SDK.
var allowedByDir = map[string][]string{
	".":               {"github.com/google/uuid"},
	"objectstoretest": {},
	"s3":              {},
	"fs":              {},
	"imgproxy":        {},
}

// TestPackageImportsAreWhitelisted — страж переносимости: ни одного импорта из
// модуля-потребителя, у каждого каталога свой белый список внешних
// зависимостей плюс stdlib. Список белый, а не чёрный, и путь модуля читается
// из go.mod, а не из литерала — иначе страж выключается после копирования
// каталога в чужой проект. _test.go не проверяются.
func TestPackageImportsAreWhitelisted(t *testing.T) {
	t.Parallel()

	module, selfImport := selfImportPath(t)
	for _, found := range scan(t, ".", module, selfImport, allowedByDir) {
		t.Errorf("%s", found)
	}
}

// TestImportGuard_Fires — проверка, что страж жив. Страж, который молчит
// всегда, выглядит ровно как страж, который работает; корпус в testdata несёт
// два разных класса нарушения, и находка обязана быть на каждом.
func TestImportGuard_Fires(t *testing.T) {
	t.Parallel()

	module, _ := selfImportPath(t)
	found := scan(t, "testdata/badcore", module, module, map[string][]string{"testdata/badcore": {}})

	// Второй класс важнее первого: соседний модуль тулкита выглядит «своим»,
	// запрет на чужую библиотеку его не ловит, а ADR-0005 разрешает внутри
	// rebar только kit, postgres адаптерам хранилища и pgtest из тестов.
	for _, want := range []string{"github.com/jackc/pgx/v5", "github.com/nrect/rebar/postgres"} {
		if !strings.Contains(strings.Join(found, "\n"), want) {
			t.Errorf("страж не сработал на импорте %s: находки %v", want, found)
		}
	}
	if len(found) != 2 {
		t.Fatalf("ожидались две находки, получено %d: %v", len(found), found)
	}

	// Каталог без записи в белом списке тоже обязан ронять тест — по находке
	// на файл: новый подпакет объявляет свои зависимости явно, а не наследует
	// чужие.
	if undeclared := scan(t, "testdata/badcore", module, module, map[string][]string{}); len(undeclared) != len(found) {
		t.Fatalf("каталог без записи в белом списке прошёл молча: %v", undeclared)
	}
}

// scan — импорты всех .go под root, сверенные с белым списком по каталогу
// (ключ — путь относительно корня модуля). Каталог testdata пропускается: он
// корпус стража, а не код пакета.
func scan(t *testing.T, root, module, selfImport string, allowed map[string][]string) []string {
	t.Helper()

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
		dir := filepath.ToSlash(filepath.Dir(name))
		list, known := allowed[dir]
		if !known {
			found = append(found, name+": каталог "+dir+" не описан в allowedByDir — "+
				"новый подпакет обязан объявить свой белый список")
			return nil
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if allowedImport(importPath, module, selfImport, list) {
				continue
			}
			found = append(found, name+":"+strconv.Itoa(fset.Position(imp.Pos()).Line)+
				" импортирует "+importPath+" — каталогу "+dir+" положены только stdlib, "+
				selfImport+" и "+strings.Join(list, ", "))
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

// selfImportPath — путь модуля и импорт-путь пакета из go.mod и положения каталога.
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
