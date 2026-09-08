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

// gooseSection — тело секции goose: строки между маркером и следующей
// директивой «-- +goose» или концом файла; ok = false, если маркера нет.
func gooseSection(sql, marker string) (body string, ok bool) {
	var out strings.Builder
	inside := false
	for line := range strings.SplitSeq(sql, "\n") {
		if directive := strings.TrimSpace(line); strings.HasPrefix(directive, "-- +goose") {
			inside = directive == marker
			ok = ok || inside
			continue
		}
		if inside {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String(), ok
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
