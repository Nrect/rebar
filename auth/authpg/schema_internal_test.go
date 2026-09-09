package authpg

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
// Без него CheckSchema тихо перестаёт проверять колонку, которую в схему
// добавили, и потребитель узнаёт о расхождении не на старте, а на первом
// запросе.
func TestExpectedSchema_MatchesFile(t *testing.T) {
	t.Parallel()

	blocks := tableBlocks(t)
	require.Len(t, blocks, len(expectedTables))

	for _, spec := range expectedTables {
		body, ok := blocks[spec.name]
		require.Truef(t, ok, "таблицы %s нет в schema.sql", spec.name)

		matches := columnLine.FindAllStringSubmatch(body, -1)
		declared := make([]string, 0, len(matches))
		for _, m := range matches {
			declared = append(declared, m[1])
		}
		require.Lenf(t, declared, len(spec.columns),
			"%s: число колонок в schema.sql и в expectedTables", spec.name)
		for _, name := range declared {
			assert.Containsf(t, spec.columns, name,
				"%s: колонка %s есть в schema.sql, но не в expectedTables", spec.name, name)
		}
		for _, name := range spec.checks {
			assert.Contains(t, Schema, "CONSTRAINT "+name+" CHECK", name)
		}
		for name, unique := range spec.indexes {
			if strings.HasSuffix(name, "_pkey") {
				// Имя первичного ключа генерирует Postgres из имени таблицы;
				// в файле его нет, зато есть само объявление PRIMARY KEY.
				assert.Contains(t, body, "PRIMARY KEY", spec.name)
				continue
			}
			kind := "CREATE INDEX "
			if unique {
				kind = "CREATE UNIQUE INDEX "
			}
			assert.Contains(t, Schema, kind+name+" ON "+spec.name, name)
		}
	}
}

// Адаптер читает и пишет ровно те колонки, которые проверяет CheckSchema.
func TestSessionColumns_AreAllExpected(t *testing.T) {
	t.Parallel()

	spec := specOf(t, tableSessions)
	for _, raw := range strings.Split(sessionColumns, ",") {
		name := strings.TrimSpace(raw)
		assert.Containsf(t, spec.columns, name, "колонка %s читается адаптером, но не проверяется", name)
	}
}

// tableBlocks — тело каждого CREATE TABLE из schema.sql.
func tableBlocks(t *testing.T) map[string]string {
	t.Helper()

	blocks := map[string]string{}
	for _, chunk := range strings.Split(Schema, "CREATE TABLE ")[1:] {
		name, body, ok := strings.Cut(chunk, " (")
		require.True(t, ok)
		body, _, ok = strings.Cut(body, "\n);")
		require.True(t, ok)
		blocks[strings.TrimSpace(name)] = body
	}
	return blocks
}

func specOf(t *testing.T, name string) tableSpec {
	t.Helper()
	for _, spec := range expectedTables {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("таблицы %s нет в expectedTables", name)
	return tableSpec{}
}
