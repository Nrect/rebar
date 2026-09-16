package inboxpg

import (
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// columnLine — объявление колонки в CREATE TABLE: четыре пробела, имя, тип с
	// заглавной; CONSTRAINT и комментарии под неё не подходят.
	columnLine     = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)
	constraintName = regexp.MustCompile(`CONSTRAINT ([a-z_]+) `)
)

// Ожидания CheckSchema живут в коде, схема — в миграциях: страж их расхождения.
// Иначе CheckSchema молча перестал бы видеть объект, добавленный новой миграцией.
func TestExpected_MatchesMigrations(t *testing.T) {
	t.Parallel()

	ddl := migrationsDDL(t)
	require.Equal(t, []string{tableEvents, tablePayloads}, slices.Sorted(maps.Keys(expected)))
	for table, spec := range expected {
		block := tableBlock(t, ddl, table)

		assert.ElementsMatch(t, slices.Collect(maps.Keys(spec.columns)), submatches(columnLine, block), "%s: колонки", table)
		assert.ElementsMatch(t, spec.constraints, submatches(constraintName, block), "%s: ограничения", table)

		indexes := regexp.MustCompile(`CREATE INDEX IF NOT EXISTS ([a-z_]+) ON `+table+` `).FindAllStringSubmatch(ddl, -1)
		assert.Len(t, indexes, len(spec.indexes), "%s: индексы", table)
		for _, m := range indexes {
			assert.Contains(t, spec.indexes, m[1], "%s: индекс", table)
		}

		triggers := map[string]string{}
		pattern := regexp.MustCompile(`CREATE TRIGGER ([a-z_]+)\s+BEFORE UPDATE ON ` + table + `\s+FOR EACH ROW EXECUTE FUNCTION ([a-z_]+)\(\)`)
		for _, m := range pattern.FindAllStringSubmatch(ddl, -1) {
			triggers[m[1]] = m[2]
			assert.Contains(t, ddl, "ALTER TABLE "+table+" ENABLE ALWAYS TRIGGER "+m[1],
				"%s объявлен, но не переведён в ENABLE ALWAYS — CheckSchema это отвергнет", m[1])
		}
		assert.Equal(t, spec.triggers, triggers, "%s: триггеры", table)
	}
	assert.Contains(t, insertEventSQL, "ON CONSTRAINT ux_inbox_events_dedup ")
	assert.Contains(t, expected[tableEvents].constraints, "ux_inbox_events_dedup", "ключ дедупа адаптера не сверяется")
}

// submatches — первая группа каждого совпадения.
func submatches(re *regexp.Regexp, text string) []string {
	matches := re.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// migrationsDDL — секции Up каталога Migrations() подряд, как их получит раннер.
func migrationsDDL(t *testing.T) string {
	t.Helper()
	migrations := Migrations()
	entries, err := fs.ReadDir(migrations, ".")
	require.NoError(t, err)
	var ddl strings.Builder
	for _, entry := range entries {
		raw, readErr := fs.ReadFile(migrations, entry.Name())
		require.NoError(t, readErr)
		up, _, found := strings.Cut(string(raw), "-- +goose Down")
		require.True(t, found, "%s: нет секции Down", entry.Name())
		ddl.WriteString(up)
	}
	return ddl.String()
}

// tableBlock — тело CREATE TABLE таблицы до закрывающей скобки.
func tableBlock(t *testing.T, ddl, table string) string {
	t.Helper()
	_, rest, found := strings.Cut(ddl, "CREATE TABLE IF NOT EXISTS "+table+" (")
	require.True(t, found, "в миграциях нет таблицы %s", table)
	block, _, found := strings.Cut(rest, "\n);")
	require.True(t, found, "у таблицы %s нет конца", table)
	return block
}
