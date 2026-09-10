package pgtest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres/pgtest"
)

const schemaSQL = `-- заголовок, не относящийся к секциям
-- +goose Up
CREATE TABLE note (id INT PRIMARY KEY, body TEXT NOT NULL);

-- +goose Down
DROP TABLE note;
`

func writeSchema(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.sql")
	require.NoError(t, os.WriteFile(path, []byte(schemaSQL), 0o600))
	return path
}

// Схема на тест — то, ради чего тесты можно писать параллельными: одно и то же
// имя таблицы в двух тестах, и ни один не видит строк другого.
func TestSchema_Isolates(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	for _, name := range []string{"первый", "второй"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.Schema(t, db)
			pgtest.Apply(t, pool, pgtest.GooseUp(t, writeSchema(t)))

			_, err := pool.Exec(t.Context(), `INSERT INTO note (id, body) VALUES (1, $1)`, name)
			require.NoError(t, err)

			var got string
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT body FROM note`).Scan(&got))
			assert.Equal(t, name, got)

			var rows int
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM note`).Scan(&rows))
			assert.Equal(t, 1, rows, "строки соседнего теста видны — search_path не изолировал")
		})
	}
}

// SchemaDSN — та же изоляция, но приложению: оно поднимает пул само по этой
// строке, и таблиц соседа не видит.
func TestSchemaDSN_Isolates(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	mine, neighbour := pgtest.SchemaDSN(t, db), pgtest.SchemaDSN(t, db)
	require.NotEqual(t, searchPathOf(t, mine), searchPathOf(t, neighbour),
		"два вызова обязаны дать разные схемы")

	minePool := appPool(t, mine)
	pgtest.Apply(t, minePool, pgtest.GooseUp(t, writeSchema(t)))
	_, err := minePool.Exec(t.Context(), `INSERT INTO note (id, body) VALUES (1, 'мой')`)
	require.NoError(t, err)

	var body string
	require.NoError(t, minePool.QueryRow(t.Context(), `SELECT body FROM note`).Scan(&body))
	assert.Equal(t, "мой", body)

	// Сосед по той же базе таблицы не видит вовсе: search_path у него свой.
	_, err = appPool(t, neighbour).Exec(t.Context(), `SELECT 1 FROM note`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42P01", pgErr.Code, "сосед видит таблицу — search_path не изолировал")
}

// appPool — пул, поднятый ПРИЛОЖЕНИЕМ по строке соединения: ровно так, как это
// делает потребитель, которому pgtest отдал DSN.
func appPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func searchPathOf(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	path := cfg.RuntimeParams["search_path"]
	require.NotEmpty(t, path, "в DSN нет search_path")
	return path
}

// GooseUp берёт только секцию наката: DROP из Down в тест не приезжает.
func TestGooseUp_TakesOnlyUpSection(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, writeSchema(t))

	assert.Contains(t, up, "CREATE TABLE note")
	assert.NotContains(t, up, "DROP TABLE")
	assert.NotContains(t, up, "заголовок")
}

func TestNow_IsUTCMicroseconds(t *testing.T) {
	t.Parallel()

	now := pgtest.Now()

	assert.Equal(t, time.UTC, now.Location())
	assert.Zero(t, now.Nanosecond()%1000, "наносекунды timestamptz не хранит")
}

// Момент, записанный в timestamptz и прочитанный обратно, обязан совпасть с
// исходным: ради этого Now и усекает.
func TestNow_SurvivesRoundTrip(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	pool := pgtest.Schema(t, db)
	pgtest.Apply(t, pool, `CREATE TABLE moment (at TIMESTAMPTZ NOT NULL)`)

	now := pgtest.Now()
	_, err := pool.Exec(t.Context(), `INSERT INTO moment (at) VALUES ($1)`, now)
	require.NoError(t, err)

	var got time.Time
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT at FROM moment`).Scan(&got))
	assert.True(t, now.Equal(got), "%s != %s", now, got)
}

// Режим TEST_DATABASE_URL: своя база на прогон, уборка брошенных, DROP на
// Close. Переменная задаётся тестом, поэтому проверяется и там, где её нет.
func TestStart_ServerMode(t *testing.T) {
	pgtest.Short(t)
	ctx := t.Context()

	admin, err := pgx.Connect(ctx, db.DSN())
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	// Брошенная база прошлого прогона и чужая база с похожим именем.
	const abandoned = "pgtest_1000000000_deadbeef"
	const foreign = "pgtest_backup"
	for _, name := range []string{abandoned, foreign} {
		_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
		require.NoError(t, err)
	}
	defer func() { _, _ = admin.Exec(context.WithoutCancel(ctx), "DROP DATABASE IF EXISTS "+foreign) }()

	t.Setenv(pgtest.EnvDatabaseURL, db.DSN())
	fresh, err := pgtest.Start(ctx, pgtest.Options{})
	require.NoError(t, err)

	cfg, err := pgx.ParseConfig(fresh.DSN())
	require.NoError(t, err)
	assert.Regexp(t, `^pgtest_\d+_[0-9a-f]{8}$`, cfg.Database)
	assert.NotEqual(t, cfg.Database, dbName(t, db.DSN()), "прогон обязан получить свою базу, а не чужую")

	assert.False(t, dbExists(t, admin, abandoned), "брошенная база старше часа не подметена")
	assert.True(t, dbExists(t, admin, foreign), "база с чужим именем подметена — sweep вышел за свой префикс")

	fresh.Close(ctx)
	assert.False(t, dbExists(t, admin, cfg.Database), "база прогона не удалена на Close")
}

// Потолок пула из Options действует и на пул СХЕМЫ, а не только на пул базы
// прогона.
//
// Схема — это то, чем пользуются тесты, и потолок нужен именно ей: адаптеру с
// несколькими параллельными транзакциями. Пока Schema брала умолчание, число,
// заданное в TestMain, молча не действовало, и упереться в потолок можно было
// только на гонке — то есть в самом дорогом месте.
func TestSchema_HonoursMaxConns(t *testing.T) {
	pgtest.Short(t)
	ctx := t.Context()

	t.Setenv(pgtest.EnvDatabaseURL, db.DSN())
	fresh, err := pgtest.Start(ctx, pgtest.Options{MaxConns: 7})
	require.NoError(t, err)
	defer fresh.Close(context.WithoutCancel(ctx))

	assert.Equal(t, int32(7), fresh.Pool().Config().MaxConns, "пул базы прогона")
	assert.Equal(t, int32(7), pgtest.Schema(t, fresh).Config().MaxConns, "пул схемы")
}

func dbExists(t *testing.T, admin *pgx.Conn, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, admin.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists))
	return exists
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	return cfg.Database
}

// Migrate применяется один раз на базу и до первого теста.
func TestStart_RunsMigrateOnce(t *testing.T) {
	pgtest.Short(t)
	ctx := t.Context()

	t.Setenv(pgtest.EnvDatabaseURL, db.DSN())
	calls := 0
	fresh, err := pgtest.Start(ctx, pgtest.Options{
		Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
			calls++
			_, execErr := pool.Exec(ctx, `CREATE TABLE note (id INT PRIMARY KEY)`)
			return execErr
		},
	})
	require.NoError(t, err)
	defer fresh.Close(ctx)

	assert.Equal(t, 1, calls)
	var exists bool
	require.NoError(t, fresh.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'note')`).Scan(&exists))
	assert.True(t, exists)
}
