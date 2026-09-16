package ledgerpg_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	var sources strings.Builder
	for _, name := range files {
		raw, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		sources.Write(raw)
	}
	for _, test := range named {
		assert.Contains(t, sources.String(), "func "+test+"(t *testing.T)", "doc.go называет стража %s, которого нет", test)
	}
}
