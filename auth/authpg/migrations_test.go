package authpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/postgres/pgtest"
)

// initMigration — первая миграция: схема целиком.
const initMigration = "00001_auth_init.sql"

// Стражи каталога и его поставка: Migrations() отдаёт ровно файлы каталога.
// Файл, который шаблон embed не взял (00002_x.SQL), молча не уехал бы
// потребителю и не накатился бы в тестах: они берут имена из Migrations().
func TestMigrations_Catalog(t *testing.T) {
	t.Parallel()

	// TODO(ADR-0011): pgtest.CheckMigrations(t, authpg.Migrations()) после тега postgres.
	_, findings := catalogFindings(authpg.Migrations())
	assert.Empty(t, findings)

	shipped, err := fs.ReadDir(authpg.Migrations(), ".")
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
		"CREATE TABLE IF NOT EXISTS auth_sessions",
		"CREATE TABLE IF NOT EXISTS auth_tokens",
		"CREATE TABLE IF NOT EXISTS auth_login_attempts",
		// Имена выданы ADR-0003: код разбирает конфликт по имени, и
		// переименование ломает потребителя молча.
		"auth_sessions_idle_chk",
		"ix_auth_sessions_subject",
		"ix_auth_sessions_expires",
		"ix_auth_tokens_subject",
		"ix_auth_tokens_expires",
		"ix_auth_login_attempts_key",
		"ix_auth_login_attempts_at",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	assert.NotContains(t, up, "REFERENCES",
		"пакет не знает имени таблицы пользователей: FK добавляет потребитель")
	// Идемпотентность через DROP TABLE стёрла бы живые сессии и токены у базы
	// со старой копией.
	assert.NotContains(t, up, "DROP TABLE")

	for _, want := range []string{
		"DROP TABLE IF EXISTS auth_login_attempts",
		"DROP TABLE IF EXISTS auth_tokens",
		"DROP TABLE IF EXISTS auth_sessions",
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
	store := authpg.New(pool)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NotEmpty(t, schemaObjects(t, pool), "без объектов после наката пустота ниже ничего не докажет")

	applyDown(t, pool)
	assert.Empty(t, schemaObjects(t, pool), "откат снимает таблицы и их индексы")
	applyDown(t, pool) // повторный откат не падает (ADR-0011, уточнение 6)

	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()))
}

// Повторный накат на схему, которая уже стоит, — так переходит база со старой
// копией схемы (ADR-0011, страж 5). Накат проходит, данные на месте, CheckSchema
// зелёный. Строка в каждой таблице несущая: DROP TABLE перед CREATE TABLE IF
// NOT EXISTS проходит и накат, и CheckSchema — ловит его только сверка строк.
func TestMigrations_ReapplyOnAppliedSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	now := pgtest.Now()
	require.NoError(t, store.Insert(t.Context(), testSession(now)))
	require.NoError(t, store.Tokens().Insert(t.Context(), tokenRow(uuid.New(), token.PurposeVerify, now)))
	require.NoError(t, store.Record(t.Context(), session.Attempt{
		ID: uuid.New(), Realm: testRealm, LoginKey: "alice@example.invalid", IP: "203.0.113.7", At: now,
	}))
	rows := tableRows(t, pool)
	require.Equal(t, [3]int{1, 1, 1}, rows, "строка в каждой таблице auth_*")

	applyUp(t, pool)

	require.NoError(t, store.CheckSchema(t.Context()))
	assert.Equal(t, rows, tableRows(t, pool), "повторный накат не тронул данные")
}

// tableRows — строк в auth_sessions, auth_tokens и auth_login_attempts.
func tableRows(t *testing.T, pool *pgxpool.Pool) [3]int {
	t.Helper()
	var n [3]int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM auth_sessions), (SELECT count(*) FROM auth_tokens),
		(SELECT count(*) FROM auth_login_attempts)`,
	).Scan(&n[0], &n[1], &n[2]))
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
