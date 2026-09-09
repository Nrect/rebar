package mailpg_test

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

// gooseDown — тело обратной секции. Секцию Up отдаёт pgtest.GooseUp; вторая
// копия разбора завела бы вторую правду о строке директивы, а в схемах тулкита
// Down последняя, поэтому это всё, что идёт после её маркера. Допущение
// проверяет TestSchemaFile_HoldsContract: без него переставленные секции
// отдали бы обрезанное тело вместо падения.
func gooseDown(t *testing.T) string {
	t.Helper()
	_, down, ok := strings.Cut(readSchema(t), pgtest.GooseDownMarker)
	require.True(t, ok, "в %s нет маркера %q", schemaPath, pgtest.GooseDownMarker)
	return down
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии. Заодно это проверка допущения, на
// котором стоит gooseDown: маркеров ровно по одному и Up идёт раньше Down.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw := readSchema(t)
	up := pgtest.GooseUp(t, schemaPath)
	down := gooseDown(t)

	require.Equal(t, 1, strings.Count(raw, pgtest.GooseUpMarker), "маркер Up обязан быть один")
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseDownMarker), "маркер Down обязан быть один")
	require.Less(t, strings.Index(raw, pgtest.GooseUpMarker), strings.Index(raw, pgtest.GooseDownMarker),
		"Down обязана идти после Up: на этом стоит разбор обратной секции")

	for _, want := range []string{
		"CREATE TABLE email_outbox",
		"ux_email_outbox_dedup", // имя индекса — часть контракта Store.Enqueue
		"ix_email_outbox_due",
		"ix_email_outbox_terminal",
		"email_outbox_body_cleared_chk",
		"email_outbox_lock_chk",
	} {
		assert.Contains(t, up, want)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.Contains(t, down, "DROP TABLE email_outbox")
	assert.NotContains(t, down, "CREATE TABLE")
}
