package authzpg

import (
	"strings"
	"testing"
)

// Карта ожидаемых колонок и schema.sql обязаны меняться вместе: колонка,
// добавленная в файл и забытая в карте, не проверялась бы у потребителя
// вовсе, а забытая в файле роняла бы CheckSchema на верной миграции.
func TestExpectedColumns_MatchSchemaFile(t *testing.T) {
	t.Parallel()

	for name := range expectedColumns {
		if !strings.Contains(Schema, "\n    "+name+" ") {
			t.Errorf("колонка %s есть в expectedColumns, но не в schema.sql", name)
		}
	}
	for _, name := range columnNames(t, Schema) {
		if _, ok := expectedColumns[name]; !ok {
			t.Errorf("колонка %s есть в schema.sql, но не в expectedColumns", name)
		}
	}
}

// Ожидаемые ограничения и индексы тоже живут в файле: имя, на которое
// ссылается код, — контракт (CONVENTIONS §9).
func TestExpectedNames_AreInSchemaFile(t *testing.T) {
	t.Parallel()

	for _, name := range expectedConstraints {
		if !strings.Contains(Schema, name) {
			t.Errorf("ограничения %s нет в schema.sql", name)
		}
	}
	for name := range expectedIndexes {
		if !strings.Contains(Schema, name) {
			t.Errorf("индекса %s нет в schema.sql", name)
		}
	}
}

// columnNames — имена колонок из CREATE TABLE: строки вида «    имя ТИП».
func columnNames(t *testing.T, schema string) []string {
	t.Helper()

	_, rest, ok := strings.Cut(schema, "CREATE TABLE "+tableName+" (")
	if !ok {
		t.Fatal("в schema.sql нет CREATE TABLE " + tableName)
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
		t.Fatal("в schema.sql не разобрана ни одна колонка")
	}
	return out
}
