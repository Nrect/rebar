package mailpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_mail_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, mailpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(mailpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(mailpg.Migrations(), ".")
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
		"CREATE TABLE IF NOT EXISTS email_outbox",
		"ux_email_outbox_dedup", // имя индекса — часть контракта Store.Enqueue
		"ix_email_outbox_due",
		"ix_email_outbox_terminal",
		"email_outbox_status_chk", // имя — контракт: по нему разбирают конфликт
		"email_outbox_fail_reason_chk",
		"email_outbox_attempts_chk",
		"email_outbox_body_cleared_chk",
		"email_outbox_lock_chk",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	// Идемпотентность через DROP TABLE стёрла бы неотправленные письма у базы со
	// старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	assert.Contains(t, down, "DROP TABLE IF EXISTS email_outbox")
	assert.NotContains(t, down, "CREATE TABLE")
}

// Накат на пустую схему, откат и снова накат (ADR-0011, стражи 4 и 6): после
// наката CheckSchema зелёный, после отката в схеме пусто, и повторный откат
// проходит — стенды гоняют Up и Down по кругу.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := mailpg.New(pool)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицу вместе с индексами")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так переходит база со старой
// копией схемы (ADR-0011, страж 5). Накат проходит, CheckSchema зелёный, и
// письма на месте: DROP TABLE перед CREATE оставил бы CheckSchema зелёным, а
// очередь пустой.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	mustEnqueue(t, store, envelope())
	const outboxRows = `SELECT count(*) FROM email_outbox`
	rows := countRows(t, pool, outboxRows)
	require.Equal(t, 1, rows, "строка в email_outbox")

	applyUp(t, pool)

	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, countRows(t, pool, outboxRows), "повторный накат не тронул данные")
}

// CHECK ⊇ AllStatuses (CONVENTIONS §9): словарь кода и словарь базы обязаны
// совпадать, иначе расхождение всплывает в проде на первом новом статусе —
// база отобьёт строку, которую домен считает законной. Тест стал возможен
// только после того, как ограничению дали имя: адресовать безымянный CHECK
// нечем.
func TestInitMigration_ChecksMirrorClosedSets(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, filepath.Join(migrationsDir, initMigration))

	statuses := checkValues(t, up, "email_outbox_status_chk")
	for _, st := range mail.AllStatuses {
		assert.Contains(t, statuses, string(st), "статус %q не зеркалится CHECK", st)
	}
	assert.Len(t, statuses, len(mail.AllStatuses), "CHECK и AllStatuses разъехались")

	// У причин отказа CHECK ⊇ набора, а не равен ему: в колонке живёт ещё
	// пустая строка — «не падало». В AllFailReasons её нет и быть не должно,
	// это не причина отказа; поэтому лишнее проверяется поимённо, а не длиной.
	// Иначе следующий добавит '' в AllFailReasons ради зелёного теста и сломает
	// домен: пустая причина стала бы законным исходом Finish.
	reasons := checkValues(t, up, "email_outbox_fail_reason_chk")
	for _, r := range mail.AllFailReasons {
		assert.Contains(t, reasons, string(r), "причина отказа %q не зеркалится CHECK", r)
	}
	assert.ElementsMatch(t, []string{""}, extra(reasons, mail.AllFailReasons),
		"сверх AllFailReasons в CHECK допустима ровно пустая строка")
}

// extra — значения CHECK, которых нет в закрытом наборе домена.
func extra[T ~string](values []string, closed []T) []string {
	known := make(map[string]bool, len(closed))
	for _, c := range closed {
		known[string(c)] = true
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !known[v] {
			out = append(out, v)
		}
	}
	return out
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
