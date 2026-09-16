package errstest

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// Находки EverySentinelNamesItsPackage.
const (
	msgNoPackagePrefix = "текст начинается не с %q: KindError равны по классу И ТЕКСТУ, и sentinel'ы двух пакетов совпали бы через errors.Is"
	msgTextNotLiteral  = "текст собран не литералом — префикс проверить нечем: сообщение пишется строкой на месте объявления"
)

func everySentinelNamesItsPackage(rep reporter, root string, allow []string) {
	walkSentinelSpecs(rep, root, allow, func(file *ast.File) sentinelVisitor {
		guard := prefixGuard{imports: importNamesOf(file), pkg: file.Name.Name}
		return func(fset *token.FileSet, rel string, _ *ast.GenDecl, spec *ast.ValueSpec) {
			guard.reportSpec(rep, fset, rel, spec)
		}
	})
}

// prefixGuard — вердикт о префиксе: имена импортов файла и имя его пакета из
// объявления package. Имя каталога не годится: у auth/authhttp пакет зовётся
// authhttp, а у payment/prorate — prorate.
type prefixGuard struct {
	imports importNames
	pkg     string
}

// reportSpec — sentinel'ы одного объявления. Директивы у стража нет, поэтому
// GenDecl ему не нужен.
func (g prefixGuard) reportSpec(rep reporter, fset *token.FileSet, rel string, spec *ast.ValueSpec) {
	for i, ident := range spec.Names {
		if !ident.IsExported() {
			continue
		}
		message, checked := g.messageOf(valueOf(spec, i))
		if !checked {
			continue
		}
		problem := g.verdict(message)
		if problem == "" {
			continue
		}
		rep.Errorf("%s:%d: %s — %s", rel, fset.Position(ident.Pos()).Line, ident.Name, problem)
	}
}

// verdict — находка по тексту sentinel ("" — префикс на месте).
func (g prefixGuard) verdict(message ast.Expr) string {
	prefix := g.pkg + ": "
	text, ok := literalText(message)
	switch {
	case !ok:
		return msgTextNotLiteral
	case !strings.HasPrefix(text, prefix):
		return fmt.Sprintf(msgNoPackagePrefix, prefix)
	}
	return ""
}

// messageOf — выражение текста sentinel и проверяется ли её префикс.
//
// Проверяются формы, у которых текст свой: errors.New, errs.Kinded и
// fmt.Errorf без %w. Не проверяются и находкой НЕ становятся три:
//
//   - обёртка fmt.Errorf("%w: …", ErrX) — её Error() начинается с текста
//     обёрнутой, и собственного префикса у неё быть не может;
//   - конструкторы SlugError (errs.New и удобства по классу) — их Error()
//     это сам слаг, а слаг kebab-case и двоеточия не допускает (ValidSlug);
//     разводит слаги CheckSlugRegistry, а не префикс;
//   - форма, которую страж не узнал: её называет EveryErrorHasKind
//     (msgUnknownForm), и второе имя той же находки читалось бы как вторая.
//
// У fmt.Errorf нелитеральный формат — тоже не находка здесь: по нему не
// узнать даже, обёртка это или нет, а EveryErrorHasKind его уже назвал.
func (g prefixGuard) messageOf(value ast.Expr) (message ast.Expr, checked bool) {
	if value == nil {
		return nil, false // объявлена без значения: это находка EveryErrorHasKind
	}
	call, isCall := ast.Unparen(value).(*ast.CallExpr)
	if !isCall {
		return nil, false
	}
	pkg, fn, ok := selectorOf(call.Fun)
	if !ok {
		return nil, false
	}
	switch {
	case pkg == g.imports.errorsPkg && fn == fnNew:
		return argAt(call, 0)
	case pkg == g.imports.errsPkg && fn == fnKinded:
		return argAt(call, 1)
	case pkg == g.imports.fmtPkg && fn == fnErrorf:
		return errorfMessage(call)
	}
	return nil, false
}

// errorfMessage — формат fmt.Errorf, если он и есть текст ошибки.
func errorfMessage(call *ast.CallExpr) (message ast.Expr, checked bool) {
	format, ok := argAt(call, 0)
	if !ok {
		return nil, false
	}
	text, isLiteral := literalText(format)
	if !isLiteral || wrapsCause(text) {
		return nil, false
	}
	return format, true
}
