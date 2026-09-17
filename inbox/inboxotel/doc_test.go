package inboxotel_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Стражи, названные в doc.go, существуют: переименованный тест иначе оставил бы
// пункт «Безопасность:» без стража, и никто бы этого не увидел.
func TestDoc_NamesExistingGuards(t *testing.T) {
	t.Parallel()

	doc, err := os.ReadFile("doc.go")
	require.NoError(t, err)
	named := regexp.MustCompile(`\bTest\w+`).FindAllString(string(doc), -1)
	require.NotEmpty(t, named, "doc.go не называет ни одного стража")

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	testFunc := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	declared := map[string]bool{}
	for _, name := range files {
		raw, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		for _, m := range testFunc.FindAllStringSubmatch(string(raw), -1) {
			declared[m[1]] = true
		}
	}
	for _, test := range named {
		assert.True(t, declared[test], "doc.go называет стража %s, которого нет", test)
	}
}
