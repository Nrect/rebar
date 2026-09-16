package paymentpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_payment_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, paymentpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(paymentpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(paymentpg.Migrations(), ".")
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
	// применялась бы без триггеров книги, а тесты зеленели бы на ней впустую.
	assert.Contains(t, up, "RETURN NEW", "тело функции не обрывается на StatementBegin")
	assert.NotContains(t, up, "+goose", "директивы раннера в тело не попадают")

	for _, want := range []string{
		// Идемпотентные формы: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS payment_intents",
		"CREATE TABLE IF NOT EXISTS payment_intent_items",
		"CREATE TABLE IF NOT EXISTS payment_events",
		"CREATE TABLE IF NOT EXISTS payment_ledger",
		"CREATE OR REPLACE FUNCTION payment_ledger_immutable()",
		"CREATE OR REPLACE FUNCTION payment_ledger_refund_cap()",
		// Имена, по которым адаптер разбирает конфликт.
		"ux_payment_intents_key",
		"ux_payment_intents_live_reference",
		"ux_payment_events_dedup",
		"ux_payment_ledger_capture",
		"ux_payment_ledger_key",
		// Книгу держит база, а не код; пересозданный триггер приходит в ORIGIN.
		"DROP TRIGGER IF EXISTS payment_ledger_immutable_trg ON payment_ledger",
		"DROP TRIGGER IF EXISTS payment_ledger_no_truncate_trg ON payment_ledger",
		"DROP TRIGGER IF EXISTS payment_ledger_refund_cap_trg ON payment_ledger",
		"ENABLE ALWAYS TRIGGER payment_ledger_immutable_trg",
		"ENABLE ALWAYS TRIGGER payment_ledger_no_truncate_trg",
		"ENABLE ALWAYS TRIGGER payment_ledger_refund_cap_trg",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Идемпотентность через DROP TABLE стёрла бы книгу у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	for _, want := range []string{
		"DROP TABLE IF EXISTS payment_ledger",
		"DROP TABLE IF EXISTS payment_events",
		"DROP TABLE IF EXISTS payment_intent_items",
		"DROP TABLE IF EXISTS payment_intents",
		"DROP FUNCTION IF EXISTS payment_ledger_refund_cap",
		"DROP FUNCTION IF EXISTS payment_ledger_immutable",
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
	store := paymentpg.New(pool, paymentpg.Options{})

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицы и функции триггеров")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так накатывается стенд или
// база со старой копией, которую не отметили накатанной (ADR-0011, стражи 5 и 7). Накат проходит, данные на месте,
// CheckSchema зелёный, и триггеры книги снова ENABLE ALWAYS — даже если режим
// сбросили: пересозданный триггер приходит в ORIGIN, и 'A' возвращает только
// безусловный ALTER следом.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	settleIntent(t, store, mustCreate(t, store, intent()))
	rows := tableRows(t, pool)
	require.Equal(t, [4]int{1, 1, 1, 1}, rows, "строка в каждой таблице payment_*")

	_, err := pool.Exec(t.Context(), `
		ALTER TABLE payment_ledger ENABLE TRIGGER payment_ledger_immutable_trg;
		ALTER TABLE payment_ledger ENABLE REPLICA TRIGGER payment_ledger_no_truncate_trg;
		ALTER TABLE payment_ledger DISABLE TRIGGER payment_ledger_refund_cap_trg`)
	require.NoError(t, err)

	applyUp(t, pool)

	assert.Equal(t, map[string]string{
		"payment_ledger_immutable_trg":   "A",
		"payment_ledger_no_truncate_trg": "A",
		"payment_ledger_refund_cap_trg":  "A",
	}, triggerModes(t, pool), "tgenabled после повторного наката")
	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, tableRows(t, pool), "повторный накат не тронул данные")
}

// triggerModes — tgenabled триггеров книги прямо из pg_trigger, мимо
// CheckSchema: страж не опирается на проверяемый код.
func triggerModes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT tgname, tgenabled::text FROM pg_trigger
		WHERE tgrelid = 'payment_ledger'::regclass AND NOT tgisinternal`)
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

// tableRows — строк в payment_intents, payment_intent_items, payment_events и
// payment_ledger.
func tableRows(t *testing.T, pool *pgxpool.Pool) [4]int {
	t.Helper()
	var n [4]int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM payment_intents), (SELECT count(*) FROM payment_intent_items),
		(SELECT count(*) FROM payment_events), (SELECT count(*) FROM payment_ledger)`,
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
