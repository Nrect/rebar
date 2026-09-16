package outboxpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_outbox_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, outboxpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(outboxpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(outboxpg.Migrations(), ".")
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
		"CREATE TABLE IF NOT EXISTS outbox_messages",
		"ux_outbox_messages_dedup", // имя индекса — часть контракта Store.Enqueue
		"ix_outbox_messages_due",
		"ix_outbox_messages_terminal",
		"ix_outbox_messages_failed",
		"ix_outbox_messages_aggregate",
		"outbox_messages_claim_chk",
		"outbox_messages_fail_chk",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имя схемы не зашито и FK на таблицы потребителя нет: search_path
	// выбирает потребитель, а его таблицу пользователей пакет не знает.
	assert.NotContains(t, up, "public.")
	assert.NotContains(t, up, "REFERENCES")
	// Идемпотентность через DROP TABLE стёрла бы очередь у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	assert.Contains(t, down, "DROP TABLE IF EXISTS outbox_messages")
	assert.NotContains(t, down, "CREATE TABLE")
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := outboxpg.New(pool)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицу и её индексы")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит (ADR-0011, страж 5): так
// накатывается стенд или база со старой копией схемы, которую не отметили
// накатанной. Накат проходит, данные на месте, CheckSchema зелёный.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	tx := beginTx(t, pool)
	res, err := store.WithTx(tx).Enqueue(t.Context(), envelope())
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	require.NoError(t, tx.Commit(t.Context()))
	rows := countRows(t, pool, `SELECT count(*) FROM outbox_messages`)
	require.Equal(t, 1, rows, "строка в outbox_messages")

	applyUp(t, pool)

	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, countRows(t, pool, `SELECT count(*) FROM outbox_messages`),
		"повторный накат не тронул данные")
}

// entryNames — имена записей каталога.
func entryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
