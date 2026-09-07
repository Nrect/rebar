package kit_test

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

// TestPackageImportsAreWhitelisted — страж переносимости, строже, чем в mail:
// у каталогов kit белый список внешних зависимостей ПУСТ, разрешены только
// stdlib и собственный модуль. Появилась внешняя зависимость — пакет уезжает
// в свой модуль (см. doc.go), а не дописывается сюда. Путь модуля читается из
// go.mod, а не из литерала, иначе страж выключается после копирования
// каталога в чужой проект. _test.go и testdata не проверяются.
func TestPackageImportsAreWhitelisted(t *testing.T) {
	t.Parallel()

	module, selfImport := selfImportPath(t)
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// testdata компилятор не собирает: там лежат фикстуры errstest.
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(name))
		allowed, known := allowedByDir[dir]
		if !known {
			t.Errorf("%s: каталог %q не описан в allowedByDir — новый подпакет обязан объявить свой белый список", name, dir)
			return nil
		}
		if len(allowed) > 0 {
			t.Errorf("каталог %q объявил внешние зависимости %v — у пакетов kit их нет: вынеси пакет в отдельный модуль", dir, allowed)
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
			t.Errorf("%s:%d импортирует %s — каталогу %q положены только stdlib и %s; вынеси зависимость в порт",
				name, fset.Position(imp.Pos()).Line, importPath, dir, selfImport)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// allowedByDir — карта каталогов модуля. Значения пусты и обязаны такими
// остаться: список нужен только для того, чтобы новый подпакет пришлось
// объявить осознанно. Каталоги secrets, ratelimit и retry вписаны заранее.
var allowedByDir = map[string][]string{
	".":                       {},
	"errs":                    {},
	"errs/httperr":            {},
	"errs/errstest":           {},
	"reqid":                   {},
	"config":                  {},
	"secrets":                 {},
	"ratelimit":               {},
	"ratelimit/ratelimithttp": {},
	"retry":                   {},
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
