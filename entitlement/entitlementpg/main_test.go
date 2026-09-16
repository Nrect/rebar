package entitlementpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд
// общий с остальными адаптерами тулкита (postgres/pgtest) и умеет
// TEST_DATABASE_URL, без которого мутант поднимал бы по контейнеру.
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

// item — предмет тестов адаптера.
const item = "course.algebra"

// moment — момент выдачи. Время в порту — параметр, часы тестам не нужны.
func moment() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }

// newStore — схема на тест плюс накат каталога миграций: тестируется артефакт,
// который уедет раннеру потребителя, а не его копия в коде.
func newStore(t *testing.T) (*entitlementpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	applyUp(t, pool)
	return entitlementpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// beginTx — транзакция потребителя. Откат в Cleanup: тест, упавший до своего
// Rollback, иначе вешал бы pool.Close на занятом соединении до таймаута.
func beginTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// countRows — сколько выдач в таблице: проверка идёт мимо адаптера.
func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM entitlement_grants`).Scan(&n))
	return n
}
