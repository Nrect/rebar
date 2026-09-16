package entitlementpg

import (
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
)

// Карта ожидаемых колонок и миграции обязаны меняться вместе: колонка, забытая
// в карте, не проверялась бы у потребителя, а забытая в миграциях роняла бы
// CheckSchema на накатанной ими базе.
func TestExpectedColumns_MatchMigrations(t *testing.T) {
	t.Parallel()

	assert.ElementsMatch(t, slices.Collect(maps.Keys(expectedColumns)), columnNames(t))
}

// ИМЕНА — КОНТРАКТ: на первое встаёт ON CONFLICT, второе отличает негодный
// предмет от сбоя. Сверка с литералами ловит и переименование в коде и в
// миграциях разом: сверку «код ↔ миграции» оно прошло бы, а базу, уже
// накатанную потребителем, сломало бы молча.
func TestContractNames_ArePinned(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "entitlement_grants", tableName)
	assert.Equal(t, "ux_entitlement_grants_subject_item", uxSubjectItem)
	assert.Equal(t, "ck_entitlement_grants_item_id", ckItemID)

	ddl := schemaDDL(t)
	for _, name := range expectedConstraints {
		assert.Contains(t, ddl, "CONSTRAINT "+name, "ограничения нет в миграциях")
	}
	for name := range expectedIndexes {
		assert.Contains(t, ddl, name, "индекса нет в миграциях")
	}
	assert.Contains(t, grantSQL, "ON CONFLICT ON CONSTRAINT "+uxSubjectItem+" DO UPDATE")
}

// ПОТОЛОК В CHECK — ТОТ ЖЕ, ЧТО В ЯДРЕ. Разъехавшуюся пару роняет тест, а не
// прод: мягче ядра — выдача в обход ядра, строже — упавшая законная выдача из
// хука платежей.
func TestSchemaCheck_MirrorsMaxItemIDLen(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	found := regexp.MustCompile(`octet_length\(item_id\) <= (\d+)`).FindAllStringSubmatch(ddl, -1)
	require.Len(t, found, 1, "потолок предмета обязан стоять в миграциях ровно один раз")
	ceiling, err := strconv.Atoi(found[0][1])
	require.NoError(t, err)
	assert.Equal(t, entitlement.MaxItemIDLen, ceiling)
	assert.Contains(t, ddl, "item_id <> ''", "пустой предмет вёл бы себя как шаблон")
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

// columnNames — имена колонок из CREATE TABLE: строки с отступом ровно в
// четыре пробела; комментарии, ограничения и их продолжения пропускаются.
func columnNames(t *testing.T) []string {
	t.Helper()

	_, rest, ok := strings.Cut(schemaDDL(t), "CREATE TABLE "+tableName+" (")
	require.True(t, ok, "в миграциях нет CREATE TABLE "+tableName)
	var out []string
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.HasPrefix(line, ")") {
			break
		}
		field, indented := strings.CutPrefix(line, "    ")
		if !indented || strings.HasPrefix(field, " ") || strings.HasPrefix(field, "--") ||
			strings.HasPrefix(field, "CONSTRAINT") {
			continue
		}
		name, _, _ := strings.Cut(field, " ")
		out = append(out, name)
	}
	require.NotEmpty(t, out, "в миграциях не разобрана ни одна колонка")
	return out
}
