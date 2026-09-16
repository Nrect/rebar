package errstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nrect/rebar/kit/errs"
)

// errsImportPath — путь errs таким, каким его собрал потребитель: после
// копирования kit в чужой модуль литерал пути молча выключил бы узнавание.
var errsImportPath = reflect.TypeFor[errs.KindError]().PkgPath()

// noKindDirective — отказ от класса с доводом, строкой над объявлением.
const noKindDirective = "//errs:nokind"

// Имена конструкторов, по которым узнаётся форма объявления. fnNew носят и
// errors.New, и errs.New: разводит их пакет, а не имя.
const (
	fnNew    = "New"
	fnErrorf = "Errorf"
	fnKinded = "Kinded"
)

// Находки EveryErrorHasKind.
const (
	msgNoValue        = "объявлена без значения — класса нет"
	msgErrorsNew      = "errors.New без класса: объяви через errs.Kinded(errs.Kind…, \"…\")"
	msgErrorfNoWrap   = "fmt.Errorf без %w — класса нет: объяви через errs.Kinded или оберни sentinel с классом"
	msgKindUnknown    = "класс errs.KindUnknown — это отсутствие класса"
	msgUnknownCtor    = "errs.Unknown — класс KindUnknown, то есть класса нет"
	msgKindErrorLit   = "errs.KindError собрана литералом — у нулевого значения класса нет: объяви через errs.Kinded"
	msgUnknownForm    = "класс по исходнику не определить: узнаются errs.Kinded, конструкторы errs и обёртка fmt.Errorf(\"%w: …\", ErrX)"
	msgNoKindNoReason = "//errs:nokind без довода — отказ от класса требует причины: //errs:nokind <почему класса нет>"
	msgNoKindHasKind  = "//errs:nokind при классе — два способа сказать одно: убери директиву (у обёртки %w класс у обёрнутой)"
)

// importNames — локальные имена импортов, из которых собирают sentinel; "" —
// не импортирован. Псевдоним узнаётся по пути, а не по имени.
type importNames struct {
	errorsPkg, fmtPkg, errsPkg string
}

func everyErrorHasKind(rep reporter, root string, allow []string) {
	walkSentinelSpecs(rep, root, allow, func(file *ast.File) sentinelVisitor {
		imports := importNamesOf(file)
		return func(fset *token.FileSet, rel string, gen *ast.GenDecl, spec *ast.ValueSpec) {
			imports.reportSpec(rep, fset, rel, gen, spec)
		}
	})
}

// sentinelVisitor — вердикт стража об одном объявлении var.
type sentinelVisitor func(fset *token.FileSet, rel string, gen *ast.GenDecl, spec *ast.ValueSpec)

// walkSentinelSpecs — package-level var под root: обход, разбор и цикл по
// объявлениям общие у стражей sentinel'ов, свой у каждого только вердикт. Две
// копии этого цикла были бы двумя способами разойтись в том, что считается
// объявлением sentinel. newVisitor зовётся раз на файл: имя пакета и имена
// импортов вердикт получает до того, как увидит первое объявление.
func walkSentinelSpecs(rep reporter, root string, allow []string, newVisitor func(file *ast.File) sentinelVisitor) {
	walkErr := walkGoFiles(root, allow, func(name, rel string) error {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		visit := newVisitor(file)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				if values, isValue := spec.(*ast.ValueSpec); isValue {
					visit(fset, rel, gen, values)
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		rep.Fatalf("errstest: обход %s: %v", root, walkErr)
	}
}

// reportSpec — sentinel'ы одного объявления: ErrA, ErrB = a, b делится по
// индексу, директива над объявлением относится ко всем его именам.
func (im importNames) reportSpec(rep reporter, fset *token.FileSet, rel string, gen *ast.GenDecl, spec *ast.ValueSpec) {
	reason, refused := noKindOf(gen, spec)
	for i, ident := range spec.Names {
		if !ident.IsExported() {
			continue
		}
		problem, known := im.verdict(valueOf(spec, i))
		if !known && !isErrName(ident.Name) {
			continue // экспортируемая переменная другого рода
		}
		if problem = withNoKind(problem, reason, refused); problem == "" {
			continue
		}
		rep.Errorf("%s:%d: %s — %s", rel, fset.Position(ident.Pos()).Line, ident.Name, problem)
	}
}

// noKindOf — директива //errs:nokind над объявлением и её довод. У var без
// скобок комментарий висит на всём объявлении, в блоке var ( … ) — на своей
// строке: директива над блоком не прячет его будущие sentinel'ы.
func noKindOf(gen *ast.GenDecl, spec *ast.ValueSpec) (reason string, found bool) {
	doc := spec.Doc
	if doc == nil && !gen.Lparen.IsValid() {
		doc = gen.Doc
	}
	if doc == nil {
		return "", false
	}
	for _, comment := range doc.List {
		fields := strings.Fields(comment.Text)
		if fields[0] == noKindDirective {
			return strings.Join(fields[1:], " "), true
		}
	}
	return "", false
}

// withNoKind — находка с учётом отказа от класса, по образцу authz.Rule{Public,
// Why}: отказ без довода — своя находка, отказ при классе — тоже, отказ с
// доводом снимает находку «класса нет».
func withNoKind(problem, reason string, refused bool) string {
	if !refused {
		return problem
	}
	if reason == "" {
		return msgNoKindNoReason
	}
	if problem == "" {
		return msgNoKindHasKind
	}
	return ""
}

// valueOf — значение i-го имени; nil — значения нет. Одно значение на
// несколько имён — многозначный вызов, его форма достаётся каждому.
func valueOf(spec *ast.ValueSpec, i int) ast.Expr {
	if len(spec.Values) == 0 {
		return nil
	}
	if len(spec.Values) == len(spec.Names) {
		return spec.Values[i]
	}
	return spec.Values[0]
}

// verdict — почему у значения нет класса ("" — класс есть) и узнана ли в нём
// ошибка: переменную без имени Err… с неузнанным значением не проверяем.
func (im importNames) verdict(value ast.Expr) (problem string, known bool) {
	if value == nil {
		return msgNoValue, false
	}
	switch v := ast.Unparen(value).(type) {
	case *ast.CallExpr:
		return im.callVerdict(v)
	case *ast.CompositeLit:
		return im.literalVerdict(v)
	}
	return msgUnknownForm, false
}

func (im importNames) callVerdict(call *ast.CallExpr) (problem string, known bool) {
	pkg, fn, ok := selectorOf(call.Fun)
	if !ok {
		return msgUnknownForm, false
	}
	if pkg == im.errorsPkg && fn == fnNew {
		return msgErrorsNew, true
	}
	if pkg == im.fmtPkg && fn == fnErrorf {
		return errorfVerdict(call)
	}
	if pkg == im.errsPkg {
		return errsVerdict(call, fn, im.errsPkg)
	}
	return msgUnknownForm, false
}

// errorfVerdict — fmt.Errorf с %w — обёртка: класс у обёрнутой ошибки, её
// сторожит собственный пакет. Без %w класса нет.
func errorfVerdict(call *ast.CallExpr) (problem string, known bool) {
	format, ok := argAt(call, 0)
	if !ok {
		return msgUnknownForm, false
	}
	text, isLiteral := literalText(format)
	if !isLiteral {
		return msgUnknownForm, false
	}
	if wrapsCause(text) {
		return "", true
	}
	return msgErrorfNoWrap, true
}

// wrapsCause — формат fmt.Errorf оборачивает причину: %w, а не экранированный
// %%w. Одно определение на оба стража: класс такой ошибки берётся у обёрнутой,
// и текст её Error() тоже начинается с текста обёрнутой.
func wrapsCause(format string) bool {
	return strings.Contains(strings.ReplaceAll(format, "%%", ""), "%w")
}

// argAt — i-й аргумент вызова; false — аргументов меньше. Страж читает
// исходник без проверки типов, поэтому короткий вызов не обязан существовать.
func argAt(call *ast.CallExpr, i int) (ast.Expr, bool) {
	if i >= len(call.Args) {
		return nil, false
	}
	return call.Args[i], true
}

// literalText — текст строкового литерала; false — выражение собрано не
// литералом (константа, склейка, вызов). Отдельной проверки token.STRING нет:
// Unquote отвергает все прочие литералы, кроме рунного, а рунным сообщение
// объявить нельзя — такой файл не соберётся, и гейт покраснеет до стража.
func literalText(expr ast.Expr) (text string, ok bool) {
	lit, isLit := ast.Unparen(expr).(*ast.BasicLit)
	if !isLit {
		return "", false
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return unquoted, true
}

// errsVerdict — Kinded и New несут класс первым аргументом, конструкторы-удобства
// — своим именем.
func errsVerdict(call *ast.CallExpr, fn, errsPkg string) (problem string, known bool) {
	if fn == fnKinded || fn == fnNew {
		return kindArgVerdict(call, errsPkg), true
	}
	kind, ok := constructorKind(fn)
	if !ok {
		return msgUnknownForm, false
	}
	if kind == errs.KindUnknown {
		return msgUnknownCtor, true
	}
	return "", true
}

// kindArgVerdict — KindUnknown первым аргументом — не класс. Прочие значения
// проверяет сам конструктор на старте.
func kindArgVerdict(call *ast.CallExpr, errsPkg string) string {
	if len(call.Args) == 0 {
		return msgUnknownForm
	}
	pkg, name, ok := selectorOf(call.Args[0])
	if ok && pkg == errsPkg && name == "KindUnknown" {
		return msgKindUnknown
	}
	return ""
}

// literalVerdict — KindError снаружи errs собирается литералом только пустой:
// поля закрыты, класса у неё нет.
func (im importNames) literalVerdict(lit *ast.CompositeLit) (problem string, known bool) {
	pkg, name, ok := selectorOf(lit.Type)
	if ok && pkg == im.errsPkg && name == "KindError" {
		return msgKindErrorLit, true
	}
	return msgUnknownForm, false
}

// constructorKind — класс конструктора-удобства errs по имени: NotFound ↔
// "not-found". Таблицы нет: новый класс с конструктором узнаётся сам, а
// правило имени сторожит тест.
func constructorKind(fn string) (kind errs.Kind, ok bool) {
	for _, k := range errs.AllKinds {
		if pascalCase(string(k)) == fn {
			return k, true
		}
	}
	return "", false
}

// pascalCase — "not-found" → "NotFound".
func pascalCase(kebab string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(kebab, "-") {
		first, size := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(first))
		b.WriteString(part[size:])
	}
	return b.String()
}

// selectorOf — X.Sel, где X — имя импорта.
func selectorOf(expr ast.Expr) (pkg, name string, ok bool) {
	sel, isSelector := expr.(*ast.SelectorExpr)
	if !isSelector {
		return "", "", false
	}
	x, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return "", "", false
	}
	return x.Name, sel.Sel.Name, true
}

// isErrName — имя вида ErrFoo: так staticcheck ST1012 зовёт экспортируемую ошибку.
func isErrName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Err")
	if !ok {
		return false
	}
	first, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsUpper(first)
}

// importNamesOf — локальные имена errors, fmt и errs в файле.
func importNamesOf(file *ast.File) importNames {
	var im importNames
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		if importPath == "errors" {
			im.errorsPkg = local
		}
		if importPath == "fmt" {
			im.fmtPkg = local
		}
		if importPath == errsImportPath {
			im.errsPkg = local
		}
	}
	return im
}
