package mailpg

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// columnLine — строка объявления колонки в CREATE TABLE: четыре пробела, имя,
// тип с заглавной; CONSTRAINT, комментарии и переносы CHECK под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// Ожидания CheckSchema живут в коде, схема — в миграциях: страж их расхождения.
func TestExpectedColumns_MatchMigrations(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	matches := columnLine.FindAllStringSubmatch(ddl, -1)
	declared := make([]string, 0, len(matches))
	for _, m := range matches {
		declared = append(declared, m[1])
	}
	require.Len(t, declared, len(expectedColumns), "число колонок в миграциях и в expectedColumns")
	for _, name := range declared {
		assert.Contains(t, expectedColumns, name, "колонка %s есть в миграциях, но не в expectedColumns", name)
	}
	for _, name := range expectedChecks {
		assert.Contains(t, ddl, "CONSTRAINT "+name+" CHECK", name)
	}
	for name, unique := range expectedIndexes {
		kind := "CREATE INDEX "
		if unique {
			kind = "CREATE UNIQUE INDEX "
		}
		assert.Contains(t, ddl, kind+name+" ON email_outbox", name)
	}
}

// schemaDDL — каталог Migrations() текстом подряд, как его получит раннер
// потребителя. IF NOT EXISTS снят: здесь сверяются имена, а форму команд держат
// TestInitMigration_HoldsContract и повторный накат.
func schemaDDL(t *testing.T) string {
	t.Helper()
	migrations := Migrations()
	entries, err := fs.ReadDir(migrations, ".")
	require.NoError(t, err)
	var ddl strings.Builder
	for _, entry := range entries {
		raw, readErr := fs.ReadFile(migrations, entry.Name())
		require.NoError(t, readErr)
		ddl.Write(raw)
		ddl.WriteString("\n")
	}
	return strings.ReplaceAll(ddl.String(), " IF NOT EXISTS ", " ")
}

// Адаптер читает и пишет ровно те колонки, которые проверяет CheckSchema.
func TestEnvelopeColumns_AreAllExpected(t *testing.T) {
	t.Parallel()

	raw := strings.Split(envelopeColumns, ",")
	columns := make([]string, 0, len(raw))
	for _, name := range raw {
		columns = append(columns, strings.TrimSpace(name))
	}
	require.Len(t, columns, len(expectedColumns))
	for _, name := range columns {
		assert.Contains(t, expectedColumns, name)
	}
}
