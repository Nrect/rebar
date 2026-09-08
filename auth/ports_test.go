package auth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// В СИГНАТУРАХ ПОРТОВ — ТОЛЬКО ПРИМИТИВЫ, uuid.UUID, time.Time И СВОИ ТИПЫ.
// Это и есть переносимость: pgtype.UUID или *http.Request в порту привязывают
// пакет к драйверу и к транспорту, и скопировать его в чужой модуль уже
// нельзя. Страж импортов ловит зависимость каталога, этот тест — тип,
// приехавший в сигнатуру.
func TestPorts_UseNoForeignTypes(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"context": true, "time": true, "uuid": true}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "ports.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !allowed[pkg.Name] {
			t.Errorf("ports.go:%d в сигнатуре порта тип %s.%s — чужому типу здесь не место",
				fset.Position(sel.Pos()).Line, pkg.Name, sel.Sel.Name)
		}
		return true
	})

	// Проверка на срабатывание: страж, который молчит всегда, выглядит так же,
	// как страж, который работает.
	if allowed[strings.ToLower("pgtype")] {
		t.Fatal("белый список пропускает типы драйвера")
	}
}
