package authhttp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// СРАВНЕНИЕ CSRF — ПОСТОЯННОГО ВРЕМЕНИ, И ЭТО ПРОВЕРЯЕТСЯ ЧТЕНИЕМ КОДА, А НЕ
// СЕКУНДОМЕРОМ. Замер времени на ноутбуке под -race — это мигающий тест, а
// мигающий тест травит весь прогон мутантов (docs/CHIP.md). Утверждение здесь
// детерминированное: в сравнении обязан стоять subtle.ConstantTimeCompare, а
// обычного == между значениями куки и заголовка быть не должно — оно выходит
// из цикла на первом несовпавшем байте, и токен подбирается по байту за раз.
func TestCSRF_ComparisonIsConstantTime(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("middleware.go")
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkConstantTime(t, string(source)); len(problems) != 0 {
		t.Fatalf("сравнение CSRF перестало быть постоянным по времени: %v", problems)
	}

	// Поведение: длина, первый байт и последний байт — всё это отказ.
	const good = "dGhpcy1pcy1hLWNzcmYtdG9rZW4"
	for name, sent := range map[string]string{
		"пусто":            "",
		"короче":           good[:len(good)-1],
		"длиннее":          good + "x",
		"другой первый":    "X" + good[1:],
		"другой последний": good[:len(good)-1] + "X",
	} {
		if csrfMatches(good, sent) {
			t.Errorf("%s: несовпадающий токен принят", name)
		}
	}
	if !csrfMatches(good, good) {
		t.Fatal("совпадающий токен обязан быть принят")
	}
}

// TestCSRF_ConstantTimeGuardFires — проверка, что страж жив: на наивной
// реализации находка обязана быть. Страж, который молчит всегда, выглядит
// ровно как страж, который работает.
func TestCSRF_ConstantTimeGuardFires(t *testing.T) {
	t.Parallel()

	for name, source := range map[string]string{
		"обычное сравнение": "package authhttp\nfunc csrfMatches(cookie, header string) bool { return cookie == header }\n",
		"сравнение через !=": "package authhttp\nfunc csrfMatches(cookie, header string) bool {\n" +
			"\tif cookie != header {\n\t\treturn false\n\t}\n\treturn true\n}\n",
		"чужая функция сравнения": "package authhttp\nimport \"strings\"\n" +
			"func csrfMatches(cookie, header string) bool { return strings.EqualFold(cookie, header) }\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if problems := checkConstantTime(t, source); len(problems) == 0 {
				t.Fatalf("страж не сработал на наивной реализации:\n%s", source)
			}
		})
	}
}

// checkConstantTime — правило целиком: в теле csrfMatches обязан быть вызов
// subtle.ConstantTimeCompare и не должно быть сравнения самих значений.
func checkConstantTime(t *testing.T, source string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "middleware.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := funcBody(file, "csrfMatches")
	if body == nil {
		return []string{"функции csrfMatches нет — сравнение переехало, и страж смотрит не туда"}
	}

	var problems []string
	var sawConstantTime bool
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if isConstantTimeCompare(node.Fun) {
				sawConstantTime = true
			}
		case *ast.BinaryExpr:
			// == и != законны только против результата сравнения (… == 1).
			if (node.Op == token.EQL || node.Op == token.NEQ) && !comparesToNumber(node) {
				problems = append(problems, "значения сравниваются оператором "+node.Op.String())
			}
		}
		return true
	})
	if !sawConstantTime {
		problems = append(problems, "в сравнении нет subtle.ConstantTimeCompare")
	}
	return problems
}

func funcBody(file *ast.File, name string) *ast.BlockStmt {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn.Body
		}
	}
	return nil
}

func isConstantTimeCompare(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare"
}

// comparesToNumber — сравнение с числовым литералом: `… == 1` у
// ConstantTimeCompare законно, а `cookie == header` нет.
func comparesToNumber(expr *ast.BinaryExpr) bool {
	for _, side := range []ast.Expr{expr.X, expr.Y} {
		lit, ok := side.(*ast.BasicLit)
		if ok && lit.Kind == token.INT {
			return true
		}
	}
	return false
}

// Страж обязан читать ИМЕННО тот файл, где живёт сравнение: переименование
// файла молча выключило бы его.
func TestCSRF_GuardReadsTheRealFile(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("middleware.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "func csrfMatches(") {
		t.Fatal("csrfMatches переехал из middleware.go — страж смотрит не туда")
	}
}
