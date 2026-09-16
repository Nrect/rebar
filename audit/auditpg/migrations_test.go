package auditpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_audit_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, auditpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(auditpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(auditpg.Migrations(), ".")
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
	// применялась бы без триггера журнала, а тесты зеленели бы на ней впустую.
	// RETURN в теле нет: оно из одного RAISE.
	assert.Contains(t, up, "RAISE EXCEPTION 'audit_events is append-only", "тело функции не обрывается на StatementBegin")
	assert.NotContains(t, up, "+goose", "директивы раннера в тело не попадают")

	for _, want := range []string{
		// Идемпотентные формы: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS audit_events",
		"CREATE OR REPLACE FUNCTION audit_events_deny_update()",
		// Имена, которые сверяет CheckSchema.
		"ix_audit_events_occurred_at",
		"ix_audit_events_actor",
		"ix_audit_events_target",
		"audit_events_outcome_chk",
		"audit_events_actor_kind_chk",
		// Журнал держит база, а не код; пересозданный триггер приходит в ORIGIN.
		"DROP TRIGGER IF EXISTS audit_events_append_only_trg ON audit_events",
		"ENABLE ALWAYS TRIGGER audit_events_append_only_trg",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Без FK на таблицы потребителя: их имён пакет не знает.
	assert.NotContains(t, up, "REFERENCES")
	// Идемпотентность через DROP TABLE стёрла бы журнал у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	for _, want := range []string{
		"DROP TABLE IF EXISTS audit_events",
		"DROP FUNCTION IF EXISTS audit_events_deny_update",
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
	sink := auditpg.New(pool)

	applyUp(t, pool)
	require.NoError(t, sink.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицу и функцию триггера")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, sink.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так переходит база со старой
// копией схемы (ADR-0011, стражи 5 и 7). Накат проходит, данные на месте,
// CheckSchema зелёный, и триггер журнала снова ENABLE ALWAYS — даже если режим
// сбросили: пересозданный триггер приходит в ORIGIN, и 'A' возвращает только
// безусловный ALTER следом.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	require.NoError(t, sink.Write(t.Context(), testEvent()))
	rows := tableRows(t, pool)
	require.Equal(t, 1, rows, "строка в audit_events")

	// Сброс несущий: на триггере в 'A' условное создание неотличимо от безусловного.
	_, err := pool.Exec(t.Context(), `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only_trg`)
	require.NoError(t, err)

	applyUp(t, pool)

	assert.Equal(t, map[string]string{
		"audit_events_append_only_trg": "A",
	}, triggerModes(t, pool), "tgenabled после повторного наката")
	require.NoError(t, sink.CheckSchema(t.Context()))
	assert.Equal(t, rows, tableRows(t, pool), "повторный накат не тронул данные")
}

// CHECK ⊇ All*: словарь кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestInitMigration_ChecksMirrorClosedSets(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, filepath.Join(migrationsDir, initMigration))

	outcomes := checkValues(t, up, "audit_events_outcome_chk")
	for _, o := range audit.AllOutcomes {
		assert.Contains(t, outcomes, string(o), "исход %q не зеркалится CHECK", o)
	}
	assert.Len(t, outcomes, len(audit.AllOutcomes), "CHECK и AllOutcomes разъехались")

	kinds := checkValues(t, up, "audit_events_actor_kind_chk")
	for _, k := range audit.AllActorKinds {
		assert.Contains(t, kinds, string(k), "род актора %q не зеркалится CHECK", k)
	}
	assert.Len(t, kinds, len(audit.AllActorKinds), "CHECK и AllActorKinds разъехались")

	// У action CHECK'а нет намеренно: набор задаёт Config.Actions потребителя.
	assert.NotContains(t, up, "audit_events_action_chk")
}

// triggerModes — tgenabled триггеров журнала прямо из pg_trigger, мимо
// CheckSchema: страж не опирается на проверяемый код.
func triggerModes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT tgname, tgenabled::text FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal`)
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

// tableRows — строк в audit_events.
func tableRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM audit_events`)
}

// checkValues — литералы из IN (...) именованного CHECK.
func checkValues(t *testing.T, sql, constraint string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(sql, "CONSTRAINT "+constraint)
	require.True(t, ok, "в схеме нет ограничения %s", constraint)
	_, rest, ok = strings.Cut(rest, "IN (")
	require.True(t, ok, "у %s нет списка IN (...)", constraint)
	list, _, ok := strings.Cut(rest, ")")
	require.True(t, ok)

	items := strings.Split(list, ",")
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, strings.Trim(strings.TrimSpace(item), "'"))
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
