package inboxpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_inbox_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, inboxpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(inboxpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(inboxpg.Migrations(), ".")
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

	// Тело функции с ';' внутри обёрнуто в StatementBegin/End для раннера
	// потребителя; разбор pgtest их не считает границей секции, иначе схема
	// применялась бы без триггеров, а тесты зеленели бы на ней впустую.
	assert.Contains(t, up, "USING ERRCODE = '23514', CONSTRAINT = 'inbox_append_only'",
		"тело функции не обрывается на StatementBegin")
	assert.NotContains(t, up, "+goose", "директивы раннера в тело не попадают")

	for _, want := range []string{
		// Идемпотентные формы: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS inbox_events",
		"CREATE TABLE IF NOT EXISTS inbox_payloads",
		"CREATE INDEX IF NOT EXISTS ix_inbox_events_received ON inbox_events (received_at)",
		"CREATE INDEX IF NOT EXISTS ix_inbox_payloads_received ON inbox_payloads (received_at)",
		"CREATE OR REPLACE FUNCTION inbox_append_only()",
		// Имена, по которым адаптер разбирает конфликт и отказ.
		"CONSTRAINT ux_inbox_events_dedup PRIMARY KEY (source, event_id)",
		"CONSTRAINT ux_inbox_payloads_event PRIMARY KEY (source, event_id)",
		"inbox_events_source_chk", "inbox_events_id_chk", "inbox_events_type_chk",
		"inbox_events_digest_chk", "inbox_events_occurred_chk", "inbox_payloads_size_chk",
		// Тело не переживает отметку.
		"REFERENCES inbox_events (source, event_id) ON DELETE CASCADE",
		// Правку держит база, а не код; пересозданный триггер приходит в ORIGIN.
		"DROP TRIGGER IF EXISTS inbox_events_append_only_trg ON inbox_events",
		"DROP TRIGGER IF EXISTS inbox_payloads_append_only_trg ON inbox_payloads",
		"ALTER TABLE inbox_events ENABLE ALWAYS TRIGGER inbox_events_append_only_trg",
		"ALTER TABLE inbox_payloads ENABLE ALWAYS TRIGGER inbox_payloads_append_only_trg",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() — вторая правда о времени.
	assert.NotContains(t, strings.ToLower(up), "now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Внешний ключ — только свой: таблиц потребителя пакет не знает.
	assert.Equal(t, strings.Count(up, "REFERENCES"), strings.Count(up, "REFERENCES inbox_events "))
	// Идемпотентность через DROP TABLE стёрла бы отметки у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	for _, want := range []string{
		"DROP TABLE IF EXISTS inbox_payloads",
		"DROP TABLE IF EXISTS inbox_events",
		"DROP FUNCTION IF EXISTS inbox_append_only()",
	} {
		assert.Contains(t, down, want)
	}
	assert.NotContains(t, down, "CREATE TABLE")
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := inboxpg.New(pool, only(nop))

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицы и функцию триггеров")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так накатывается стенд или
// база, которую не отметили накатанной (ADR-0011, стражи 5 и 7). Накат
// проходит, строки на месте, CheckSchema зелёный, и триггеры снова ENABLE
// ALWAYS — режимы сброшены заранее: на триггере в 'A' условное создание
// неотличимо от безусловного.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	mustAccept(t, store, testEvent("evt-reapply", "reapply"), inbox.OutcomeAccepted)
	rows := tableRows(t, pool)
	require.Equal(t, [2]int{1, 1}, rows, "строка в каждой таблице inbox_*")

	_, err := pool.Exec(t.Context(), `
		ALTER TABLE inbox_events ENABLE REPLICA TRIGGER inbox_events_append_only_trg;
		ALTER TABLE inbox_payloads DISABLE TRIGGER inbox_payloads_append_only_trg`)
	require.NoError(t, err)

	applyUp(t, pool)

	assert.Equal(t, map[string]string{
		"inbox_events_append_only_trg":   "A",
		"inbox_payloads_append_only_trg": "A",
	}, triggerModes(t, pool), "tgenabled после повторного наката")
	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, tableRows(t, pool), "повторный накат не тронул данные")
}

// tableRows — строк в inbox_events и inbox_payloads.
func tableRows(t *testing.T, pool *pgxpool.Pool) [2]int {
	t.Helper()
	var n [2]int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM inbox_events), (SELECT count(*) FROM inbox_payloads)`).Scan(&n[0], &n[1]))
	return n
}

// entryNames — имена записей каталога.
func entryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
