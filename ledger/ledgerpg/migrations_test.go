package ledgerpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger/ledgerpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_ledger_init.sql"

// ledgerTriggers — триггеры схемы: на журнале и на счетах.
var ledgerTriggers = []string{
	"ledger_accounts_guard_trg", "ledger_accounts_no_truncate_trg",
	"ledger_entries_apply_trg", "ledger_entries_check_trg",
	"ledger_entries_immutable_trg", "ledger_entries_no_truncate_trg",
}

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, ledgerpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(ledgerpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(ledgerpg.Migrations(), ".")
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

	// Тела функций с ';' внутри обёрнуты в StatementBegin/End для раннера
	// потребителя; разбор pgtest их не считает границей секции, иначе схема
	// применялась бы без триггеров, а тесты зеленели бы на ней впустую.
	assert.Contains(t, up, "RETURN NEW", "тело функции не обрывается на StatementBegin")
	assert.NotContains(t, up, "+goose", "директивы раннера в тело не попадают")

	for _, want := range []string{
		// Идемпотентные формы: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS ledger_books",
		"CREATE TABLE IF NOT EXISTS ledger_kinds",
		"CREATE TABLE IF NOT EXISTS ledger_accounts",
		"CREATE TABLE IF NOT EXISTS ledger_entries",
		"CREATE UNIQUE INDEX IF NOT EXISTS ux_ledger_entries_reversal",
		"CREATE OR REPLACE FUNCTION ledger_entries_check()",
		"CREATE OR REPLACE FUNCTION ledger_entries_apply()",
		"CREATE OR REPLACE FUNCTION ledger_entries_immutable()",
		"CREATE OR REPLACE FUNCTION ledger_accounts_guard()",
		// Имена, по которым адаптер разбирает отказ.
		"ux_ledger_entries_seq", "ux_ledger_entries_key", "ledger_entries_kind_fkey", "ledger_entries_floor",
		// Остаток пишет функция с правами владельца и закреплённым search_path.
		"LANGUAGE plpgsql SECURITY DEFINER AS",
		"SET search_path = %I, pg_temp",
	} {
		assert.Contains(t, up, want)
	}
	// Журнал и голову держит база, а не код; пересозданный триггер приходит в
	// ORIGIN, поэтому на каждый — три команды подряд.
	for _, trigger := range ledgerTriggers {
		table := "ledger_entries"
		if strings.HasPrefix(trigger, "ledger_accounts") {
			table = "ledger_accounts"
		}
		assert.Regexp(t, `DROP TRIGGER IF EXISTS `+trigger+` ON `+table+`;\nCREATE TRIGGER `+trigger+
			`\n[^;]+;\nALTER TABLE `+table+` ENABLE ALWAYS TRIGGER `+trigger+`;`, up, trigger)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Идемпотентность через DROP TABLE стёрла бы журнал у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")
	assert.NotContains(t, up, "CREATE OR REPLACE TRIGGER", "сбрасывает режим в ORIGIN")

	for _, want := range []string{
		"DROP TABLE IF EXISTS ledger_entries",
		"DROP TABLE IF EXISTS ledger_accounts",
		"DROP TABLE IF EXISTS ledger_kinds",
		"DROP TABLE IF EXISTS ledger_books",
		"DROP FUNCTION IF EXISTS ledger_accounts_guard",
		"DROP FUNCTION IF EXISTS ledger_entries_immutable",
		"DROP FUNCTION IF EXISTS ledger_entries_apply",
		"DROP FUNCTION IF EXISTS ledger_entries_check",
	} {
		assert.Contains(t, down, want)
	}
	assert.NotContains(t, down, "CREATE TABLE")
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу. Справочник пишет миграция
// потребителя, поэтому он заводится после каждого наката.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := ledgerpg.New(pool, wallet())

	applyUp(t, pool)
	seedBook(t, pool, wallet())
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицы и функции триггеров")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	seedBook(t, pool, wallet())
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит (ADR-0011, стражи 5 и 7; условие
// выпуска 7 ADR-0009). Накат проходит, данные на месте, CheckSchema зелёный, и
// все триггеры снова ENABLE ALWAYS — по pg_trigger, а не по имени, и даже если
// режим сбросили: пересозданный триггер приходит в ORIGIN, и 'A' возвращает
// только безусловный ALTER следом.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 1000, "reapply-in"))
	rows := tableRows(t, pool)
	require.Equal(t, [4]int{1, 4, 1, 1}, rows, "строка в каждой таблице ledger_*: книга, её роды, счёт и запись")

	_, err := pool.Exec(t.Context(), `
		ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_check_trg;
		ALTER TABLE ledger_entries ENABLE REPLICA TRIGGER ledger_entries_apply_trg;
		ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_immutable_trg;
		ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_no_truncate_trg;
		ALTER TABLE ledger_accounts ENABLE REPLICA TRIGGER ledger_accounts_guard_trg;
		ALTER TABLE ledger_accounts DISABLE TRIGGER ledger_accounts_no_truncate_trg`)
	require.NoError(t, err)
	for trigger, mode := range triggerModes(t, pool) {
		require.NotEqual(t, "A", mode, "контроль: режим %s сброшен", trigger)
	}

	applyUp(t, pool)

	want := map[string]string{}
	for _, trigger := range ledgerTriggers {
		want[trigger] = "A"
	}
	assert.Equal(t, want, triggerModes(t, pool), "tgenabled после повторного наката")
	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, tableRows(t, pool), "повторный накат не тронул данные")

	mustPost(t, svc, spend(account, 400, "reapply-after"))
	requireChain(t, svc, account, 2)
}

// triggerModes — tgenabled триггеров схемы прямо из pg_trigger, мимо
// CheckSchema: страж не опирается на проверяемый код.
func triggerModes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT tgname, tgenabled::text FROM pg_trigger
		WHERE tgrelid IN ('ledger_entries'::regclass, 'ledger_accounts'::regclass) AND NOT tgisinternal`)
	require.NoError(t, err)
	defer rows.Close()
	modes := map[string]string{}
	for rows.Next() {
		var name, mode string
		require.NoError(t, rows.Scan(&name, &mode))
		modes[name] = mode
	}
	require.NoError(t, rows.Err())
	return modes
}

// tableRows — строк в ledger_books, ledger_kinds, ledger_accounts и ledger_entries.
func tableRows(t *testing.T, pool *pgxpool.Pool) [4]int {
	t.Helper()
	var n [4]int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM ledger_books), (SELECT count(*) FROM ledger_kinds),
		(SELECT count(*) FROM ledger_accounts), (SELECT count(*) FROM ledger_entries)`,
	).Scan(&n[0], &n[1], &n[2], &n[3]))
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

// search_path функций закрепляется за схемой миграции через format('%I'): имя
// схемы, которому нужны кавычки, накат не ломает, и триггеры находят таблицы.
func TestMigrations_QuotedSchemaName(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	quoted := pgx.Identifier{"Ledger_" + uuid.NewString()[:8]}.Sanitize()
	_, err := pool.Exec(t.Context(), "CREATE SCHEMA "+quoted)
	require.NoError(t, err)

	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = quoted
	quotedPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(quotedPool.Close)

	applyUp(t, quotedPool)
	require.NotEmpty(t, schemaObjects(t, quotedPool), "накат лёг в схему с кавычками")
	seedBook(t, quotedPool, wallet())
	store := ledgerpg.New(quotedPool, wallet())
	require.NoError(t, store.CheckSchema(t.Context()))
	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 100, "quoted"))
	requireChain(t, svc, account, 1)
}
