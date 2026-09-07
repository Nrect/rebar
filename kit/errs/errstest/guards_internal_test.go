package errstest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder — двойник reporter: копит находки, вместо того чтобы ронять тест.
// Fatalf у него не прерывает выполнение, поэтому в стражах он стоит последним.
type recorder struct {
	errors []string
	fatals []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recorder) joined() string { return strings.Join(r.errors, "\n") }

func TestNoDirectHTTPErrors_CleanTreePasses(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	noDirectHTTPErrors(rec, filepath.Join("testdata", "good"), nil)

	assert.Empty(t, rec.errors, "форма ошибки в комментарии — не нарушение")
	assert.Empty(t, rec.fatals)
}

func TestNoDirectHTTPErrors_FindsEveryViolation(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	noDirectHTTPErrors(rec, filepath.Join("testdata", "bad"), nil)

	require.Len(t, rec.errors, 3, "vendor под корнем не смотрим: чужой код всё равно не наш")
	joined := rec.joined()
	assert.Contains(t, joined, "handler.go:9: http.Error")
	assert.Contains(t, joined, `handler.go:13: рукописное тело ошибки ("slug":`)
	assert.Contains(t, joined, `handler.go:17: рукописное тело ошибки ("error":`)
}

// Каталог, названный testdata, пропускается — но если он и есть корень
// проверки, пропускать нечего: иначе страж молча не смотрел бы ничего.
func TestNoDirectHTTPErrors_RootIsNeverSkipped(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	noDirectHTTPErrors(rec, "testdata", nil)

	assert.NotEmpty(t, rec.errors, "нарушения под корнем видны, даже если корень зовут testdata")
}

// allow снимает проверку с файла и с каталога целиком.
func TestNoDirectHTTPErrors_Allow(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "allowed")

	rec := &recorder{}
	noDirectHTTPErrors(rec, root, nil)
	require.Len(t, rec.errors, 1, "без allow нарушение видно")

	for name, allow := range map[string][]string{
		"файл":      {"legacy/old.go"},
		"каталог":   {"legacy"},
		"со слэшем": {"legacy/"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			noDirectHTTPErrors(rec, root, allow)
			assert.Empty(t, rec.errors)
		})
	}
}

func TestNoDirectHTTPErrors_MissingRootIsFatal(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	noDirectHTTPErrors(rec, filepath.Join("testdata", "нет-такого"), nil)

	assert.Empty(t, rec.errors)
	assert.Len(t, rec.fatals, 1)
}

func TestCheckSlugRegistry(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	checkSlugRegistry(rec, []string{"user-not-found", "pack-already-owned"})
	assert.Empty(t, rec.errors)

	rec = &recorder{}
	checkSlugRegistry(rec, []string{"user-not-found", "User Not Found", "user-not-found", ""})
	require.Len(t, rec.errors, 3)
	joined := rec.joined()
	assert.Contains(t, joined, `"User Not Found" не kebab-case`)
	assert.Contains(t, joined, `"user-not-found" объявлен дважды`)
}

func TestKindStatusTable_PassesOnRealTable(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	kindStatusTable(rec)

	assert.Empty(t, rec.errors)
}
