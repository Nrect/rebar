package entitlementpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_entitlement_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, entitlementpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(entitlementpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(entitlementpg.Migrations(), ".")
	require.NoError(t, err)
	onDisk, err := os.ReadDir(migrationsDir)
	require.NoError(t, err)
	assert.Equal(t, entryNames(onDisk), entryNames(shipped), "Migrations() отдаёт не весь каталог")
}

// Первая миграция — артефакт для раннера потребителя: имена, на которые
// опирается адаптер, и формы команд проверяются в файле, а не в копии.
func TestInitMigration_HoldsContract(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, filepath.Join(migrationsDir, initMigration))
	down := gooseDown(t, initMigration)

	for _, want := range []string{
		// Идемпотентная форма: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS entitlement_grants",
		// Имена, по которым адаптер разбирает конфликт.
		"CONSTRAINT ux_entitlement_grants_subject_item PRIMARY KEY (subject_id, item_id)",
		"CONSTRAINT ck_entitlement_grants_item_id",
	} {
		assert.Contains(t, up, want)
	}
	assert.Equal(t, 1, strings.Count(up, "CREATE TABLE"), "адаптер мигрирует одну таблицу — выдачи")
	assert.NotContains(t, up, "entitlement_product", "каталог мигрирует потребитель")
	assert.NotContains(t, strings.ToUpper(up), "REFERENCES", "FK на таблицы потребителя не бывает")
	// Время только параметром (CONVENTIONS §9): now() нет ни в DEFAULT, ни в CHECK.
	assert.NotContains(t, strings.ToLower(up), "now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Идемпотентность через DROP TABLE стёрла бы выдачи у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	assert.Contains(t, down, "DROP TABLE IF EXISTS entitlement_grants")
	assert.NotContains(t, down, "CREATE TABLE")
	// Down снимает ровно свою таблицу: без CASCADE и без каталога потребителя.
	assert.Equal(t, []string{"DROP TABLE IF EXISTS entitlement_grants;"}, statements(down))
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := entitlementpg.New(pool)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицу выдач")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так переходит база со старой
// копией схемы (ADR-0011, страж 5). Накат проходит, выдачи на месте, и
// CheckSchema зелёный.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	require.NoError(t, store.Grant(t.Context(), uuid.New(), entitlement.Grant{ItemID: item}, moment()))
	rows := countRows(t, pool)
	require.Equal(t, 1, rows, "строка в entitlement_grants")

	applyUp(t, pool)

	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, countRows(t, pool), "повторный накат не тронул данные")
}

// statements — строки SQL без комментариев и пустых.
func statements(sql string) []string {
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			out = append(out, line)
		}
	}
	return out
}

// entryNames — имена записей каталога.
func entryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
