package outboxpg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
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
		assert.Contains(t, Schema, kind+name+" ON outbox_messages", name)
	}
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

// CHECK ⊇ All*: словарь кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestSchemaChecks_MirrorClosedEnums(t *testing.T) {
	t.Parallel()

	statuses := valuesIn(t, Schema, "status IN (")
	require.Len(t, statuses, len(outbox.AllStatuses), "число статусов в CHECK и в AllStatuses")
	for _, s := range outbox.AllStatuses {
		assert.Contains(t, statuses, string(s), "статус %s не зеркалится CHECK", s)
	}

	reasons := valuesIn(t, Schema, "fail_reason IN (")
	// Пустая строка — «причины нет»: она законна во всех статусах, кроме failed.
	require.Len(t, reasons, len(outbox.AllFailReasons)+1)
	assert.Contains(t, reasons, "")
	for _, r := range outbox.AllFailReasons {
		assert.Contains(t, reasons, string(r), "причина %s не зеркалится CHECK", r)
	}
}

// valuesIn — литералы из списка `<колонка> IN ('a','b')` файла схемы.
func valuesIn(t *testing.T, schema, prefix string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(schema, prefix)
	require.True(t, ok, "в schema.sql нет %q", prefix)
	list, _, ok := strings.Cut(rest, ")")
	require.True(t, ok)

	values := make([]string, 0, strings.Count(list, ",")+1)
	for item := range strings.SplitSeq(list, ",") {
		values = append(values, strings.Trim(strings.TrimSpace(item), "'"))
	}
	return values
}
