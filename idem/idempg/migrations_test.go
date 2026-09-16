package idempg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_idem_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, idempg.Migrations()) после тега postgres.
	_, findings := catalogFindings(idempg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(idempg.Migrations(), ".")
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
		// Идемпотентные формы: накат проходит и там, где схема уже стоит.
		"CREATE TABLE IF NOT EXISTS idem_records",
		"CREATE INDEX IF NOT EXISTS ix_idem_records_created ON idem_records (created_at)",
		// Имена-контракт (решение 15): арбитр ON CONFLICT и CHECK формы.
		"CONSTRAINT ux_idem_records_key PRIMARY KEY (realm, subject, idem_key)",
		"CONSTRAINT idem_records_realm_chk",
		"CONSTRAINT idem_records_subject_chk",
		"CONSTRAINT idem_records_key_chk",
		"CONSTRAINT idem_records_operation_chk",
		"CONSTRAINT idem_records_fingerprint_chk",
		"CONSTRAINT idem_records_status_chk",
		"CONSTRAINT idem_records_body_chk",
		"content_type TEXT NOT NULL DEFAULT ''",
		"location     TEXT NOT NULL DEFAULT ''",
	} {
		assert.Contains(t, up, want)
	}
	assert.Equal(t, 1, strings.Count(up, "CREATE TABLE"), "адаптер мигрирует одну таблицу — записи")
	assert.NotContains(t, strings.ToUpper(up), "TRIGGER", "триггера нет: UPDATE в адаптере отсутствует (решение 15)")
	assert.NotContains(t, strings.ToUpper(up), "REFERENCES", "FK на таблицы потребителя не бывает")
	// Время только параметром (CONVENTIONS §9): now() нет ни в DEFAULT, ни в CHECK.
	assert.NotContains(t, strings.ToLower(up), "now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Идемпотентность через DROP TABLE стёрла бы записи у базы со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	// Down снимает ровно свою таблицу: индекс уходит вместе с ней, CASCADE нет.
	assert.Equal(t, []string{"DROP TABLE IF EXISTS idem_records;"}, statements(down))
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := idempg.New(pool, testConfig(), idemtest.NewObserver())

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицу записей")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так накатывается стенд или
// база со старой копией, которую не отметили накатанной (ADR-0011, страж 5).
// Накат проходит, записи на месте, CheckSchema зелёный, повтор отвечает
// записанным.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	req := request(t, "reapply")
	_, err := store.Do(t.Context(), req, order("reapply", created(1)))
	require.NoError(t, err)
	rows := [2]int{records(t, pool), orders(t, pool)}
	require.Equal(t, [2]int{1, 1}, rows, "запись и эффект до наката")

	applyUp(t, pool)

	require.NoError(t, store.CheckSchema(t.Context()))
	after := [2]int{records(t, pool), orders(t, pool)}
	assert.Equal(t, rows, after, "повторный накат не тронул данные")
	res, err := store.Do(t.Context(), req, notRun)
	require.NoError(t, err)
	assert.True(t, res.Replayed, "после наката повтор отвечает записанным")
	assert.Equal(t, created(1), res.Response)
}

// statements — строки SQL без комментариев и пустых.
func statements(sql string) []string {
	var out []string
	for line := range strings.SplitSeq(sql, "\n") {
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
