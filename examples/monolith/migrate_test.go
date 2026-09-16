package monolith_test

import (
	"io/fs"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"

	"github.com/nrect/rebar/examples/monolith"
	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// versionTables — таблицы версий, которые заводит накат: по одной на блок по
// имени модуля и своя у монолита (ADR-0011, решение 3). Список свой, а не из
// сборки: страж не опирается на проверяемый код.
var versionTables = []string{
	"auth_schema_version", "authz_schema_version", "mail_schema_version",
	"outbox_schema_version", "payment_schema_version", "audit_schema_version",
	"entitlement_schema_version", "shop_schema_version",
}

// foreignKeys — внешние ключи монолита на таблицы блоков.
var foreignKeys = []string{
	"fk_auth_sessions_subject", "fk_auth_tokens_subject", "fk_entitlement_grants_subject",
}

// TestMigrations_Catalog — стражи каталога монолита (ADR-0011, решение 7,
// пункты 1–3): номера подряд с 00001, у каждого файла обе секции goose.
func TestMigrations_Catalog(t *testing.T) {
	pgtest.CheckMigrations(t, monolith.Migrations())
}

// TestMigrate_EmptyBase — пустая база: каждый каталог накатан целиком своим
// провайдером со своей таблицей версий, общей goose_db_version нет, и сверка
// каждого блока зелёная — включая entitlementpg.
func TestMigrate_EmptyBase(t *testing.T) {
	sdb := migrationDB(t)
	catalogs := monolith.Catalogs(sdb, monolith.Migrations())

	applied, err := shoppg.Migrate(t.Context(), sdb.Pool, catalogs)
	require.NoError(t, err, "накат на пустую базу")
	require.Equal(t, fileCounts(t, catalogs), applied, "каждый каталог накатан целиком")
	require.ElementsMatch(t, versionTables, versionTablesIn(t, sdb.Pool))
	requireSchemaGreen(t, sdb)
}

// TestMigrate_ReapplyAppliesNothing — повторный накат не применяет ничего ни у
// одного провайдера. Секция Up монолита, выполненная заново мимо раннера,
// проходит на данных: таблицы на месте, внешние ключи сняты и поставлены снова.
func TestMigrate_ReapplyAppliesNothing(t *testing.T) {
	sdb := migrationDB(t)
	catalogs := monolith.Catalogs(sdb, monolith.Migrations())
	_, err := shoppg.Migrate(t.Context(), sdb.Pool, catalogs)
	require.NoError(t, err, "первый накат")
	subject := seedSession(t, sdb.Pool)

	applied, err := shoppg.Migrate(t.Context(), sdb.Pool, catalogs)
	require.NoError(t, err, "повторный накат")
	require.Equal(t, make([]int, len(catalogs)), applied, "ни один провайдер ничего не применил")

	// Раннер тело уже накатанной миграции не выполняет — выполняем сами.
	pgtest.ApplyUp(t, sdb.Pool, monolith.Migrations())
	require.ElementsMatch(t, foreignKeys, foreignKeysIn(t, sdb.Pool), "ключи после повторного Up")
	var sessions int
	require.NoError(t, sdb.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM auth_sessions WHERE subject_id = $1`, subject).Scan(&sessions))
	require.Equal(t, 1, sessions, "повторный Up не тронул данные")
	requireSchemaGreen(t, sdb)
}

// TestMigrate_DownTwiceThenUp — откат всех каталогов в обратном порядке дважды
// подряд, затем снова накат (ADR-0011, уточнение 6). Первый откат — раннером:
// он же снимает отметки версий. Второй — секциями Down мимо раннера: раннер без
// отметок не выполнил бы ничего, и повторный Down остался бы непроверенным.
func TestMigrate_DownTwiceThenUp(t *testing.T) {
	sdb := migrationDB(t)
	catalogs := monolith.Catalogs(sdb, monolith.Migrations())
	_, err := shoppg.Migrate(t.Context(), sdb.Pool, catalogs)
	require.NoError(t, err, "накат до отката")

	downAll(t, sdb.Pool, catalogs)
	require.Equal(t, versionObjects(), pgtest.SchemaObjects(t, sdb.Pool), "откат снял всё, кроме таблиц версий")

	for i := len(catalogs) - 1; i >= 0; i-- {
		pgtest.ApplyDown(t, sdb.Pool, catalogs[i].FS)
	}
	require.Equal(t, versionObjects(), pgtest.SchemaObjects(t, sdb.Pool), "повторный откат")

	applied, err := shoppg.Migrate(t.Context(), sdb.Pool, catalogs)
	require.NoError(t, err, "накат после двух откатов")
	require.Equal(t, fileCounts(t, catalogs), applied, "каталоги накатаны заново целиком")
	requireSchemaGreen(t, sdb)
}

// migrationDB — пустая схема теста и пул на неё, открытый так же, как сборка.
func migrationDB(t *testing.T) *shoppg.DB {
	t.Helper()
	pgtest.Short(t)
	sdb, err := shoppg.Open(t.Context(), pgtest.SchemaDSN(t, db), postgres.Config{
		LockTimeout: 3 * time.Second, StatementTimeout: 10 * time.Second,
		MaxAttempts: 1, RetryBase: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(sdb.Close)
	return sdb
}

// requireSchemaGreen — сверка каждого блока списком сборки, а entitlementpg ещё
// и мимо него.
func requireSchemaGreen(t *testing.T, sdb *shoppg.DB) {
	t.Helper()
	for _, check := range monolith.SchemaChecks(sdb) {
		require.NoError(t, check(t.Context()))
	}
	require.NoError(t, entitlementpg.New(sdb.Pool).CheckSchema(t.Context()), "выдачи сверяются по миграциям блока")
}

// downAll откатывает каталоги раннером в обратном порядке наката.
func downAll(t *testing.T, pool *pgxpool.Pool, catalogs []shoppg.Catalog) {
	t.Helper()
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	for i := len(catalogs) - 1; i >= 0; i-- {
		p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, catalogs[i].FS,
			goose.WithTableName(catalogs[i].Versions))
		require.NoError(t, err)
		_, err = p.DownTo(t.Context(), 0)
		require.NoError(t, err, "откат %s", catalogs[i].Versions)
	}
}

// fileCounts — сколько миграций в каждом каталоге.
func fileCounts(t *testing.T, catalogs []shoppg.Catalog) []int {
	t.Helper()
	counts := make([]int, 0, len(catalogs))
	for _, c := range catalogs {
		files, err := fs.Glob(c.FS, "*.sql")
		require.NoError(t, err)
		counts = append(counts, len(files))
	}
	return counts
}

// versionObjects — что остаётся в схеме после отката: таблицы версий goose с
// первичным ключом и последовательностью идентификатора.
func versionObjects() []string {
	objects := make([]string, 0, 3*len(versionTables))
	for _, table := range versionTables {
		objects = append(objects, "таблица "+table, "индекс "+table+"_pkey", "последовательность "+table+"_id_seq")
	}
	slices.Sort(objects)
	return objects
}

// versionTablesIn — таблицы версий goose в схеме теста, включая общую.
func versionTablesIn(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return namesOf(t, pool, `SELECT tablename FROM pg_tables
		WHERE schemaname = current_schema()
		  AND (tablename LIKE '%\_schema\_version' OR tablename = 'goose_db_version')`)
}

// foreignKeysIn — действующие внешние ключи fk_* в схеме теста.
func foreignKeysIn(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return namesOf(t, pool, `SELECT conname FROM pg_constraint
		WHERE contype = 'f' AND convalidated AND conname LIKE 'fk\_%'
		  AND connamespace = current_schema()::regnamespace`)
}

func namesOf(t *testing.T, pool *pgxpool.Pool, query string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), query)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

// seedSession — пользователь и его сессия: строка, которую держит внешний ключ.
func seedSession(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	subject, now := uuid.New(), pgtest.Now()
	_, err := pool.Exec(t.Context(), `INSERT INTO shop_users (id, login, password_hash, created_at, updated_at)
		VALUES ($1, 'seed@example.test', 'hash', $2, $2)`, subject, now)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO auth_sessions
		(token_hash, realm, subject_id, created_at, last_seen_at, expires_at, idle_expires_at)
		VALUES ('seed', 'shop', $1, $2, $2, $3, $3)`, subject, now, now.Add(time.Hour))
	require.NoError(t, err)
	return subject
}
