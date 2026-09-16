package idempg

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

// migrationUp — секции Up каталога Migrations() текстом подряд, как их получит
// раннер потребителя.
func migrationUp(t *testing.T) string {
	t.Helper()
	migrations := Migrations()
	entries, err := fs.ReadDir(migrations, ".")
	require.NoError(t, err)
	var up strings.Builder
	for _, entry := range entries {
		raw, readErr := fs.ReadFile(migrations, entry.Name())
		require.NoError(t, readErr)
		section, _, ok := strings.Cut(string(raw), "-- +goose Down")
		require.True(t, ok, "в %s нет секции Down", entry.Name())
		up.WriteString(section)
		up.WriteString("\n")
	}
	return up.String()
}

// columnLine — объявление колонки в CREATE TABLE: четыре пробела, имя, тип
// заглавными; CONSTRAINT и комментарии под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// Ожидания CheckSchema живут в коде, схема — в миграциях: страж их расхождения.
// Колонка, CHECK или индекс, добавленные в файл и забытые здесь, не
// проверялись бы у потребителя ничем.
func TestExpectedSchema_MatchesMigrations(t *testing.T) {
	t.Parallel()

	up := migrationUp(t)
	start := strings.Index(up, "CREATE TABLE IF NOT EXISTS "+tableName+" (")
	require.GreaterOrEqual(t, start, 0, "в миграции нет таблицы %s", tableName)
	end := strings.Index(up[start:], "\n);")
	require.GreaterOrEqual(t, end, 0, "объявление %s не закрыто", tableName)
	body := up[start : start+end]

	columns := columnLine.FindAllStringSubmatch(body, -1)
	declared := make([]string, 0, len(columns))
	for _, m := range columns {
		declared = append(declared, m[1])
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(expectedColumns)), declared, "колонки")

	named := regexp.MustCompile(`CONSTRAINT (\w+) (CHECK|PRIMARY KEY)`).FindAllStringSubmatch(body, -1)
	constraints := make([]string, 0, len(named))
	for _, m := range named {
		constraints = append(constraints, m[1])
	}
	assert.ElementsMatch(t, expectedConstraints, constraints, "первичный ключ и CHECK")

	created := regexp.MustCompile(`CREATE INDEX IF NOT EXISTS (\w+) ON `+tableName+` `).FindAllStringSubmatch(up, -1)
	indexes := make([]string, 0, len(created)+1)
	indexes = append(indexes, uxKey) // индекс первичного ключа
	for _, m := range created {
		indexes = append(indexes, m[1])
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(expectedIndexes)), indexes, "индексы")
}

// ИМЕНА — КОНТРАКТ: на первичный ключ встаёт ON CONFLICT, по индексу идёт
// уборка. Сверка с литералами ловит переименование в коде и в миграции разом:
// сверку «код ↔ миграции» оно прошло бы, а базу, накатанную потребителем,
// сломало бы молча.
func TestContractNames_ArePinned(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "idem_records", tableName)
	assert.Equal(t, "ux_idem_records_key", uxKey)
	assert.Equal(t, "ix_idem_records_created", ixCreated)
	assert.Contains(t, insertSQL, "ON CONFLICT ON CONSTRAINT "+uxKey+" DO NOTHING")
	for _, query := range []string{recordSQL, insertSQL, purgeSQL} {
		assert.Contains(t, query, tableName)
	}
}
