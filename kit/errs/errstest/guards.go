package errstest

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// handwrittenBody — рукописное тело ошибки в строковом литерале. Структурный
// тег `json:"slug"` под шаблон не подходит: двоеточия после ключа там нет.
var handwrittenBody = regexp.MustCompile(`"(slug|error)"\s*:`)

// skipDirs — каталоги, которых нет в проде.
var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, "testdata": true}

func noDirectHTTPErrors(rep reporter, root string, allow []string) {
	walkErr := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != root && skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || allowed(rel, allow) {
			return nil
		}
		return scanForDirectErrors(rep, name, rel)
	})
	if walkErr != nil {
		rep.Fatalf("errstest: обход %s: %v", root, walkErr)
	}
}

// allowed — путь начинается с одного из разрешённых префиксов (файл или каталог).
func allowed(rel string, allow []string) bool {
	for _, prefix := range allow {
		prefix = strings.TrimSuffix(filepath.ToSlash(prefix), "/")
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

// scanForDirectErrors — токены файла: вызов http.Error и строковые литералы
// с телом ошибки. Комментарии сканер не отдаёт — они и не проверяются.
func scanForDirectErrors(rep reporter, name, rel string) error {
	src, err := os.ReadFile(name) //nolint:gosec // путь приходит от теста потребителя
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	var sc scanner.Scanner
	sc.Init(fset.AddFile(rel, fset.Base(), len(src)), src, nil, 0)

	prevIdent, afterHTTPDot := "", false
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			return nil
		}
		switch {
		case afterHTTPDot && tok == token.IDENT && lit == "Error":
			rep.Errorf("%s:%d: http.Error — ответ мимо слага; верни errs.SlugError и отдай её httperr.Responder.Write",
				rel, fset.Position(pos).Line)
		case tok == token.STRING:
			if key := handwrittenBody.FindString(unquote(lit)); key != "" {
				rep.Errorf("%s:%d: рукописное тело ошибки (%s) — тело пишет только httperr.Responder.Write",
					rel, fset.Position(pos).Line, key)
			}
		}
		afterHTTPDot = tok == token.PERIOD && prevIdent == "http"
		prevIdent = ""
		if tok == token.IDENT {
			prevIdent = lit
		}
	}
}

// unquote — текст литерала; негодный литерал проверяется как есть.
func unquote(lit string) string {
	if text, err := strconv.Unquote(lit); err == nil {
		return text
	}
	return lit
}

func checkSlugRegistry(rep reporter, all []string) {
	seen := make(map[string]bool, len(all))
	for _, slug := range all {
		if !errs.ValidSlug(slug) {
			rep.Errorf("слаг %q не kebab-case или длиннее %d байт (errs.ValidSlug)", slug, errs.MaxSlugLen)
		}
		if seen[slug] {
			rep.Errorf("слаг %q объявлен дважды", slug)
		}
		seen[slug] = true
	}
}

func kindStatusTable(rep reporter) {
	for _, kind := range errs.AllKinds {
		status := httperr.StatusOf(kind)
		switch {
		case kind == errs.KindUnknown:
			if status != http.StatusInternalServerError {
				rep.Errorf("KindUnknown обязан отдавать 500, получено %d", status)
			}
		case status == http.StatusInternalServerError:
			rep.Errorf("Kind %q не описан в таблице httperr: StatusOf вернул запасные 500", kind)
		case status < http.StatusBadRequest || status >= 600:
			rep.Errorf("Kind %q даёт статус %d вне 4xx/5xx", kind, status)
		}
	}
}
