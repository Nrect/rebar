package authzpg_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// gooseSection — тело секции goose: строки между маркером и следующим
// маркером секции или концом файла; ok = false, если маркера нет. Копия из
// pgtest (ADR-0005, «Копируемые мелочи») вместе с её правкой.
func gooseSection(sql, marker string) (body string, ok bool) {
	var out strings.Builder
	inside := false
	for line := range strings.SplitSeq(sql, "\n") {
		directive := strings.TrimSpace(line)
		if !strings.HasPrefix(directive, "-- +goose") {
			if inside {
				out.WriteString(line)
				out.WriteString("\n")
			}
			continue
		}
		// СЕКЦИЮ ПЕРЕКЛЮЧАЮТ ТОЛЬКО Up И Down. Прочие директивы goose
		// (StatementBegin/StatementEnd вокруг тела функции, NO TRANSACTION)
		// — часть секции: считая их сменой секции, разбор терял бы тело
		// функции, схема применялась бы без неё, а тест на инвариант,
		// который эта функция держит, зеленел бы впустую.
		if directive == gooseUp || directive == gooseDown {
			inside = directive == marker
			ok = ok || inside
		}
	}
	return out.String(), ok
}

// Регрессия: директива внутри секции не должна её обрывать. У схемы authz
// тела функции сегодня нет, но копия разбора обязана держать тот же контракт,
// что и оригинал в pgtest, — иначе первый же триггер выпадет молча.
func TestGooseSection_KeepsStatementBlocks(t *testing.T) {
	t.Parallel()

	const doc = gooseUp + "\n" +
		"CREATE TABLE t (id INT);\n" +
		"-- +goose StatementBegin\n" +
		"CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql;\n" +
		"-- +goose StatementEnd\n" +
		gooseDown + "\n" +
		"DROP TABLE t;\n"

	up, ok := gooseSection(doc, gooseUp)
	require.True(t, ok)
	assert.Contains(t, up, "CREATE FUNCTION f()", "тело функции выпало из секции")
	assert.Contains(t, up, "CREATE TABLE t")
	assert.NotContains(t, up, "DROP TABLE")

	down, ok := gooseSection(doc, gooseDown)
	require.True(t, ok)
	assert.Contains(t, down, "DROP TABLE t")
	assert.NotContains(t, down, "CREATE FUNCTION")
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии в коде.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok)
	down, ok := gooseSection(raw, gooseDown)
	require.True(t, ok)

	for _, want := range []string{
		"CREATE TABLE authz_role_assignments",
		"authz_role_assignments_pkey", // имя — арбитр ON CONFLICT в Assign
		"authz_role_assignments_role_chk",
		"authz_role_assignments_expires_chk",
		"ix_authz_role_assignments_expires",
	} {
		assert.Contains(t, up, want)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.NotContains(t, up, "DEFAULT now()", "время приходит параметром (CONVENTIONS §9)")
	assert.NotContains(t, up, "REFERENCES", "FK на таблицы потребителя не бывает")
	assert.Contains(t, down, "DROP TABLE authz_role_assignments")
	assert.NotContains(t, down, "CREATE TABLE")
}
