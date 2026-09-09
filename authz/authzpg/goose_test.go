package authzpg_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres/pgtest"
)

// schemaPath — файл схемы; он же артефакт для goose потребителя.
const schemaPath = "schema.sql"

// readSchema — файл целиком.
func readSchema(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	require.NoError(t, err)
	return string(raw)
}

// gooseDown — тело обратной секции. pgtest отдаёт только Up, а заводить
// вторую копию разбора секций незачем: в схемах тулкита Down последняя,
// поэтому это всё, что идёт после её маркера. Единственность маркера и
// порядок секций проверяет TestSchemaFile_HoldsContract.
func gooseDown(t *testing.T) string {
	t.Helper()
	_, down, ok := strings.Cut(readSchema(t), pgtest.GooseDownMarker)
	require.True(t, ok, "в %s нет маркера %q", schemaPath, pgtest.GooseDownMarker)
	return down
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии в коде. Заодно это проверка
// допущения, на котором стоит gooseDown: маркер Down один и стоит после Up.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw := readSchema(t)
	up := pgtest.GooseUp(t, schemaPath)
	down := gooseDown(t)

	require.Equal(t, 1, strings.Count(raw, pgtest.GooseDownMarker), "маркер Down обязан быть один")
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseUpMarker), "маркер Up обязан быть один")
	require.Less(t, strings.Index(raw, pgtest.GooseUpMarker), strings.Index(raw, pgtest.GooseDownMarker),
		"Down обязана идти после Up: на этом стоит разбор обратной секции")

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
