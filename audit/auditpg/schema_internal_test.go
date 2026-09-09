package auditpg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// columnLine — строка объявления колонки в CREATE TABLE: четыре пробела, имя,
// тип с заглавной; CONSTRAINT, комментарии и переносы CHECK под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// Ожидания CheckSchema живут в коде, схема — в файле: страж их расхождения.
func TestExpectedColumns_MatchSchemaFile(t *testing.T) {
	t.Parallel()

	matches := columnLine.FindAllStringSubmatch(Schema, -1)
	declared := make([]string, 0, len(matches))
	for _, m := range matches {
		declared = append(declared, m[1])
	}
	require.Len(t, declared, len(expectedColumns), "число колонок в schema.sql и в expectedColumns")
	for _, name := range declared {
		assert.Contains(t, expectedColumns, name, "колонка %s есть в schema.sql, но не в expectedColumns", name)
	}
	for _, name := range expectedChecks {
		assert.Contains(t, Schema, "CONSTRAINT "+name+" CHECK", name)
	}
	for name, unique := range expectedIndexes {
		kind := "CREATE INDEX "
		if unique {
			kind = "CREATE UNIQUE INDEX "
		}
		assert.Contains(t, Schema, kind+name+" ON audit_events", name)
	}
	for _, name := range expectedTriggers {
		assert.Contains(t, Schema, "CREATE TRIGGER "+name, name)
		assert.Contains(t, Schema, "ENABLE ALWAYS TRIGGER "+name,
			"%s объявлен, но не переведён в ENABLE ALWAYS — CheckSchema это отвергнет", name)
	}
}

// Адаптер пишет ровно те колонки, которые проверяет CheckSchema.
func TestEventColumns_AreAllExpected(t *testing.T) {
	t.Parallel()

	raw := strings.Split(eventColumns, ",")
	columns := make([]string, 0, len(raw))
	for _, name := range raw {
		columns = append(columns, strings.TrimSpace(name))
	}
	require.Len(t, columns, len(expectedColumns))
	for _, name := range columns {
		assert.Contains(t, expectedColumns, name)
	}
}
