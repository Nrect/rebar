package entitlementpg_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement/entitlementpg"
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

// gooseDown — тело обратной секции: в схемах тулкита Down последняя, поэтому
// это всё после её маркера. Порядок и единственность маркеров проверяет
// TestSchemaFile_HoldsContract.
func gooseDown(t *testing.T) string {
	t.Helper()
	_, down, ok := strings.Cut(readSchema(t), pgtest.GooseDownMarker)
	require.True(t, ok, "в %s нет маркера %q", schemaPath, pgtest.GooseDownMarker)
	return down
}

// Схема — артефакт для goose потребителя: контракт проверяется в файле, а не в
// его копии в коде.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw := readSchema(t)
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseUpMarker), "маркер Up обязан быть один")
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseDownMarker), "маркер Down обязан быть один")
	require.Less(t, strings.Index(raw, pgtest.GooseUpMarker), strings.Index(raw, pgtest.GooseDownMarker),
		"Down обязана идти после Up: на этом стоит разбор обратной секции")

	up := pgtest.GooseUp(t, schemaPath)
	assert.Contains(t, up, "CONSTRAINT ux_entitlement_grants_subject_item PRIMARY KEY (subject_id, item_id)")
	assert.Contains(t, up, "CONSTRAINT ck_entitlement_grants_item_id")
	assert.Equal(t, 1, strings.Count(up, "CREATE TABLE"), "адаптер мигрирует одну таблицу — выдачи")
	assert.NotContains(t, up, "entitlement_product", "каталог мигрирует потребитель")
	assert.NotContains(t, strings.ToLower(up), "now()", "время приходит параметром (CONVENTIONS §9)")
	assert.NotContains(t, strings.ToUpper(up), "REFERENCES", "FK на таблицы потребителя не бывает")

	// Down снимает ровно свою таблицу: без CASCADE и без каталога потребителя.
	assert.Equal(t, "DROP TABLE entitlement_grants;", strings.TrimSpace(gooseDown(t)))
}

// Обе стороны миграции применяются на пустую базу: файл уезжает в миграции
// потребителя как есть.
func TestSchema_AppliesBothWays(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	require.NoError(t, entitlementpg.New(pool).CheckSchema(t.Context()))

	pgtest.Apply(t, pool, gooseDown(t))
	require.Error(t, entitlementpg.New(pool).CheckSchema(t.Context()), "после отката таблицы нет")
}
