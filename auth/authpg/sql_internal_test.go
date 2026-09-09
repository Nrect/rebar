package authpg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ourTables — таблицы пакета: только запросы к ним обязаны нести реалм.
// Запросы к каталогу Postgres (information_schema, pg_class) под правило не
// подпадают — там реалма нет и быть не может.
var ourTables = regexp.MustCompile(`(?i)\b(?:FROM|UPDATE|INTO|JOIN)\s+(auth_sessions|auth_tokens|auth_login_attempts)\b`)

// РЕАЛМ СТОИТ В КАЖДОМ WHERE. HMAC под секретом реалма уже разводит хэши, но
// колонка realm — вторая линия и единственный ключ уборки: запрос без неё в
// двухреалмовом процессе обслуживает чужие строки, с виду работая, а строки,
// уехавшие мимо реалма, потом нельзя ни найти, ни удалить.
//
// Страж ходит по ВСЕМ строковым константам пакета, а не по списку запросов:
// список пришлось бы дополнять руками, и запрос, забытый в нём, проходил бы
// молча — ровно так страж и умирает.
func TestSQL_HasRealmInEveryWhere(t *testing.T) {
	t.Parallel()

	statements := packageStrings(t)
	if len(statements) == 0 {
		t.Fatal("в пакете не нашлось ни одной строковой константы — страж смотрит не туда")
	}

	var sawTableQuery bool
	for name, sql := range statements {
		if !ourTables.MatchString(sql) {
			continue
		}
		sawTableQuery = true
		for _, problem := range checkRealm(sql) {
			t.Errorf("%s: %s", name, problem)
		}
	}
	if !sawTableQuery {
		t.Fatal("страж не увидел ни одного запроса к таблицам пакета — он смотрит не туда")
	}
}

// TestSQL_RealmGuardFires — проверка, что страж жив. Страж, который молчит
// всегда, выглядит ровно как страж, который работает; корпус несёт заведомые
// нарушения, и находка на каждом обязана быть.
func TestSQL_RealmGuardFires(t *testing.T) {
	t.Parallel()

	for name, sql := range map[string]string{
		"выборка без реалма":  `SELECT token_hash FROM auth_sessions WHERE token_hash = $1`,
		"удаление без реалма": `DELETE FROM auth_tokens WHERE expires_at < $1`,
		"правка без реалма":   `UPDATE auth_sessions SET last_seen_at = $2 WHERE token_hash = $1`,
		"вставка без колонки": `INSERT INTO auth_login_attempts (id, login_key, at) VALUES ($1, $2, $3)`,
		"реалм не в WHERE":    `SELECT realm FROM auth_sessions WHERE token_hash = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if problems := checkRealm(sql); len(problems) == 0 {
				t.Fatalf("страж не сработал на заведомом нарушении: %s", sql)
			}
		})
	}

	// И наоборот: законный запрос находкой быть не должен, иначе страж
	// сводится к «всегда красный» и его выключат.
	ok := `DELETE FROM auth_sessions WHERE realm = $1 AND subject_id = $2`
	if problems := checkRealm(ok); len(problems) != 0 {
		t.Fatalf("страж ругается на законный запрос: %v", problems)
	}
}

// checkRealm — правило целиком: у запроса с WHERE обязано быть `realm = $`, у
// вставки — колонка realm в списке.
func checkRealm(sql string) []string {
	var problems []string
	upper := strings.ToUpper(sql)
	if strings.Contains(upper, "WHERE") && !strings.Contains(sql, "realm = $") {
		problems = append(problems, "запрос к таблице пакета без `realm = $` в WHERE")
	}
	if strings.Contains(upper, "INSERT INTO") && !insertsRealm(sql) {
		problems = append(problems, "вставка без колонки realm")
	}
	return problems
}

// insertsRealm — есть ли realm в списке колонок INSERT. Список берётся до
// VALUES: слово realm в самом значении колонкой не является.
func insertsRealm(sql string) bool {
	head, _, found := strings.Cut(strings.ToUpper(sql), "VALUES")
	if !found {
		head = strings.ToUpper(sql)
	}
	return strings.Contains(head, "REALM")
}

// packageStrings — все строковые константы и литералы пакета с раскрытой
// конкатенацией: запросы собраны из констант (`... ` + columns + ` ...`), и
// без раскрытия страж видел бы обрубки без WHERE.
func packageStrings(t *testing.T) map[string]string {
	t.Helper()

	files := parsePackage(t)
	consts := map[string]string{}
	// Два прохода: константа может ссылаться на объявленную ниже.
	for range 2 {
		for _, file := range files {
			collectConsts(file, consts)
		}
	}

	out := map[string]string{}
	for name, value := range consts {
		out[name] = value
	}
	// Литералы берутся только из ТЕЛ ФУНКЦИЙ. В объявлениях констант запрос
	// собран из кусков (`INSERT INTO auth_sessions (` + columns + `)`), и
	// каждый кусок по отдельности выглядел бы нарушением — при том что
	// собранный запрос законен и уже проверен выше.
	for path, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				out[path+":"+strconv.Itoa(int(lit.Pos()))] = value
				return true
			})
		}
	}
	return out
}

func collectConsts(file *ast.File, consts map[string]string) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if value, ok := evalString(vs.Values[i], consts); ok {
					consts[name.Name] = value
				}
			}
		}
	}
}

// evalString раскрывает литерал, ссылку на константу и их конкатенацию.
func evalString(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := consts[e.Name]
		return value, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, okL := evalString(e.X, consts)
		right, okR := evalString(e.Y, consts)
		return left + right, okL && okR
	}
	return "", false
}

func parsePackage(t *testing.T) map[string]*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		files[name] = file
	}
	return files
}
