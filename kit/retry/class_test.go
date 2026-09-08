package retry_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/retry"
)

var errBoom = errors.New("boom")

// Чужой пакет реализует контракт, не импортируя retry: классификация обязана
// работать по методам, а не по типам обёрток.
type foreignPermanent struct{ permanent bool }

func (foreignPermanent) Error() string     { return "foreign" }
func (e foreignPermanent) Permanent() bool { return e.permanent }
func (e foreignPermanent) String() string  { return "foreign" }

type foreignThrottled struct {
	after time.Duration
	ok    bool
}

func (foreignThrottled) Error() string                       { return "foreign throttled" }
func (e foreignThrottled) RetryAfter() (time.Duration, bool) { return e.after, e.ok }

// Набор Class закрытый: каждая объявленная константа обязана быть в
// AllClasses. Проверка по исходнику, а не по счётчику: забытое значение молча
// получило бы «transient» и вечные повторы.
func TestAllClassesListsEveryConstant(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "class.go", nil, 0)
	require.NoError(t, err)

	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || !isClassSpec(value) {
				continue
			}
			declared++
			lit, ok := value.Values[0].(*ast.BasicLit)
			require.Truef(t, ok, "%s объявлен не литералом", value.Names[0].Name)
			assert.Containsf(t, retry.AllClasses, retry.Class(strings.Trim(lit.Value, `"`)),
				"константа %s не попала в AllClasses", value.Names[0].Name)
		}
	}

	assert.NotZero(t, declared, "в class.go не нашлось ни одной константы Class")
	assert.Len(t, retry.AllClasses, declared, "в AllClasses есть лишнее или повторы")
}

func isClassSpec(spec *ast.ValueSpec) bool {
	ident, ok := spec.Type.(*ast.Ident)
	return ok && ident.Name == "Class" && len(spec.Names) == 1 && len(spec.Values) == 1
}

func TestClassify(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want retry.Class
	}{
		{name: "nil — повторять нечего", err: nil, want: retry.ClassPermanent},
		{name: "голая ошибка", err: errBoom, want: retry.ClassTransient},
		{name: "постоянная", err: retry.Permanent(errBoom), want: retry.ClassPermanent},
		{name: "с просьбой подождать", err: retry.Throttled(errBoom, time.Second), want: retry.ClassThrottled},
		{name: "постоянная под обёрткой", err: fmt.Errorf("шаг: %w", retry.Permanent(errBoom)), want: retry.ClassPermanent},
		{name: "чужой тип с Permanent", err: foreignPermanent{permanent: true}, want: retry.ClassPermanent},
		{name: "чужой Permanent() == false", err: foreignPermanent{}, want: retry.ClassTransient},
		{name: "чужой RetryAfter", err: foreignThrottled{after: time.Second, ok: true}, want: retry.ClassThrottled},
		{name: "чужой RetryAfter() ok == false", err: foreignThrottled{}, want: retry.ClassTransient},
		{name: "постоянная важнее просьбы", err: retry.Permanent(retry.Throttled(errBoom, time.Second)), want: retry.ClassPermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, retry.Classify(tc.err))
		})
	}
}

func TestWrappers_KeepCauseAndText(t *testing.T) {
	t.Parallel()

	permanent := retry.Permanent(errBoom)
	require.ErrorIs(t, permanent, errBoom, "причина обязана остаться в цепочке")
	assert.Equal(t, errBoom.Error(), permanent.Error(), "обёртка не дописывает текст")

	throttled := retry.Throttled(errBoom, time.Minute)
	require.ErrorIs(t, throttled, errBoom)
	assert.Equal(t, errBoom.Error(), throttled.Error())
}

func TestWrappers_NilStaysNil(t *testing.T) {
	t.Parallel()

	assert.NoError(t, retry.Permanent(nil))
	assert.NoError(t, retry.Throttled(nil, time.Second))
}

func TestRetryAfterOf(t *testing.T) {
	t.Parallel()

	after, ok := retry.RetryAfterOf(retry.Throttled(errBoom, 90*time.Second))
	require.True(t, ok)
	assert.Equal(t, 90*time.Second, after)

	_, ok = retry.RetryAfterOf(errBoom)
	assert.False(t, ok, "у голой ошибки подсказки нет")

	_, ok = retry.RetryAfterOf(nil)
	assert.False(t, ok)

	// Отрицательный срок приводится к нулю: «уже можно», а не сон в прошлое.
	negative, ok := retry.RetryAfterOf(retry.Throttled(errBoom, -time.Hour))
	require.True(t, ok)
	assert.Zero(t, negative)
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()

	assert.True(t, retry.IsPermanent(retry.Permanent(errBoom)))
	assert.False(t, retry.IsPermanent(errBoom))
	assert.False(t, retry.IsPermanent(nil))
	assert.True(t, retry.IsPermanent(fmt.Errorf("контекст: %w", retry.Permanent(errBoom))))
}
