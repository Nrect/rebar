package entitlementpg

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Карта ожидаемых колонок и schema.sql обязаны меняться вместе: колонка,
// забытая в карте, не проверялась бы у потребителя, а забытая в файле роняла
// бы CheckSchema на верной миграции.
func TestExpectedColumns_MatchSchemaFile(t *testing.T) {
	t.Parallel()

	assert.ElementsMatch(t, slices.Collect(maps.Keys(expectedColumns)), columnNames(t))
}

// ИМЕНА — КОНТРАКТ: на первое встаёт ON CONFLICT, второе отличает негодный
// предмет от сбоя. Сверка с литералами ловит и переименование в коде и в
// файле разом: сверку «код ↔ файл» оно прошло бы, а миграцию, уже
// скопированную потребителем, сломало бы молча.
func TestContractNames_ArePinned(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "entitlement_grants", tableName)
	assert.Equal(t, "ux_entitlement_grants_subject_item", uxSubjectItem)
	assert.Equal(t, "ck_entitlement_grants_item_id", ckItemID)

	up := pgtest.GooseUp(t, "schema.sql")
	for _, name := range expectedConstraints {
		assert.Contains(t, up, "CONSTRAINT "+name, "ограничения нет в schema.sql")
	}
	for name := range expectedIndexes {
		assert.Contains(t, up, name, "индекса нет в schema.sql")
	}
	assert.Contains(t, grantSQL, "ON CONFLICT ON CONSTRAINT "+uxSubjectItem+" DO UPDATE")
}

// ПОТОЛОК В CHECK — ТОТ ЖЕ, ЧТО В ЯДРЕ. Разъехавшуюся пару роняет тест, а не
// прод: мягче ядра — выдача в обход ядра, строже — упавшая законная выдача из
// хука платежей.
func TestSchemaCheck_MirrorsMaxItemIDLen(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, "schema.sql")
	found := regexp.MustCompile(`octet_length\(item_id\) <= (\d+)`).FindAllStringSubmatch(up, -1)
	require.Len(t, found, 1, "потолок предмета обязан стоять в schema.sql ровно один раз")
	ceiling, err := strconv.Atoi(found[0][1])
	require.NoError(t, err)
	assert.Equal(t, entitlement.MaxItemIDLen, ceiling)
	assert.Contains(t, up, "item_id <> ''", "пустой предмет вёл бы себя как шаблон")
}

// columnNames — имена колонок из CREATE TABLE: строки с отступом ровно в
// четыре пробела; комментарии, ограничения и их продолжения пропускаются.
func columnNames(t *testing.T) []string {
	t.Helper()

	_, rest, ok := strings.Cut(Schema, "CREATE TABLE "+tableName+" (")
	require.True(t, ok, "в schema.sql нет CREATE TABLE "+tableName)
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
	require.NotEmpty(t, out, "в schema.sql не разобрана ни одна колонка")
	return out
}
