package mail_test

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

// TestPackageImportsAreWhitelisted — страж переносимости: ни одного импорта
// из модуля-потребителя, у каждого каталога свой белый список внешних
// зависимостей плюс stdlib. Список белый, а не чёрный, и путь модуля читается
// из go.mod, а не из литерала — иначе страж выключается после копирования
// каталога в чужой проект. _test.go не проверяются.
func TestPackageImportsAreWhitelisted(t *testing.T) {
	t.Parallel()

	module, selfImport := selfImportPath(t)
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(name))
		allowed, known := allowedByDir[dir]
		if !known {
			t.Errorf("%s: каталог %q не описан в allowedByDir — новый подпакет обязан объявить свой белый список", name, dir)
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
			t.Errorf("%s:%d импортирует %s — каталогу %q положены только stdlib, %s и %v; вынеси зависимость в порт",
				name, fset.Position(imp.Pos()).Line, importPath, dir, selfImport, allowed)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// allowedByDir — белый список внешних импортов по каталогу. Новый подпакет
// добавляется сюда тем же коммитом, что и каталог.
var allowedByDir = map[string][]string{
	".":        {"github.com/google/uuid"},
	"mailtest": {"github.com/google/uuid"},
	"mailpg":   {"github.com/google/uuid", "github.com/jackc/pgx/v5"},
	"smtp":     {"github.com/google/uuid", "github.com/wneessen/go-mail"},
	"sesv2":    {"github.com/google/uuid"},
	"mailotel": {"github.com/google/uuid", "go.opentelemetry.io/otel/metric", "go.opentelemetry.io/otel/attribute"},
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
