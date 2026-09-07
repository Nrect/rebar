package postgres_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь; таблицы каждый тест заводит в своей схеме.
var db *pgtest.DB

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	started, err := pgtest.Start(ctx, pgtest.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db = started
	code := m.Run()
	db.Close(ctx)
	os.Exit(code)
}

// testConfig — политика тестов: lock_timeout заведомо длиннее
// deadlock_timeout Postgres (1s), иначе дедлок приходил бы как 55P03.
func testConfig() postgres.Config {
	return postgres.Config{
		LockTimeout:      5 * time.Second,
		StatementTimeout: 10 * time.Second,
		MaxAttempts:      5,
		RetryBase:        5 * time.Millisecond,
	}
}

// newRunner — схема на тест, таблица счётчиков и Runner над ними.
func newRunner(t *testing.T, cfg postgres.Config) (*postgres.Runner, *pgxpool.Pool) {
	t.Helper()
	pgtest.Short(t)
	pool := pgtest.Schema(t, db)
	pgtest.Apply(t, pool, `CREATE TABLE counter (
		id   INT PRIMARY KEY,
		n    INT  NOT NULL DEFAULT 0,
		note TEXT NOT NULL DEFAULT ''
	)`)
	return postgres.New(pool, cfg), pool
}

func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM counter`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
