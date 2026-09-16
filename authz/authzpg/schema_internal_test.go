package authzpg

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Карта ожидаемых колонок и миграции обязаны меняться вместе: колонка,
// добавленная в миграцию и забытая в карте, не проверялась бы у потребителя
// вовсе, а забытая в миграции роняла бы CheckSchema на верной схеме.
func TestExpectedColumns_MatchMigrations(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	for name := range expectedColumns {
		if !strings.Contains(ddl, "\n    "+name+" ") {
			t.Errorf("колонка %s есть в expectedColumns, но не в миграциях", name)
		}
	}
	for _, name := range columnNames(t, ddl) {
		if _, ok := expectedColumns[name]; !ok {
			t.Errorf("колонка %s есть в миграциях, но не в expectedColumns", name)
		}
	}
}

// Ожидаемые ограничения и индексы тоже живут в миграциях: имя, на которое
// ссылается код, — контракт (CONVENTIONS §9).
func TestExpectedNames_AreInMigrations(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	for _, name := range expectedConstraints {
		if !strings.Contains(ddl, name) {
			t.Errorf("ограничения %s нет в миграциях", name)
		}
	}
	for name := range expectedIndexes {
		if !strings.Contains(ddl, name) {
			t.Errorf("индекса %s нет в миграциях", name)
		}
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

// columnNames — имена колонок из CREATE TABLE: строки вида «    имя ТИП».
func columnNames(t *testing.T, schema string) []string {
	t.Helper()

	_, rest, ok := strings.Cut(schema, "CREATE TABLE "+tableName+" (")
	if !ok {
		t.Fatal("в миграциях нет CREATE TABLE " + tableName)
	}
	var out []string
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ")") {
			break
		}
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(strings.TrimSpace(line), "--") ||
			strings.HasPrefix(strings.TrimSpace(line), "CONSTRAINT") {
			continue
		}
		name, _, found := strings.Cut(strings.TrimSpace(line), " ")
		if found {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		t.Fatal("в миграциях не разобрана ни одна колонка")
	}
	return out
}
