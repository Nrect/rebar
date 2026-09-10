package monolith_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/nrect/rebar/postgres/pgtest"
)

// db и box — стенд прогона: Postgres и Mailpit, по одному на тестовый бинарь.
var (
	db  *pgtest.DB
	box *mailpit
)

// TestMain поднимает стенд. По -short он не поднимается вовсе: тест сквозной,
// и без Docker ему нечего проверять.
func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run())
	}
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:forbidigo // TestMain: логгера ещё нет
		os.Exit(1)
	}
	os.Exit(code)
}

func runSuite(m *testing.M) (int, error) {
	ctx := context.Background()
	started, err := pgtest.Start(ctx, pgtest.Options{MaxConns: 10})
	if err != nil {
		return 0, fmt.Errorf("стенд: Postgres: %w", err)
	}
	db = started
	defer db.Close(ctx)

	mp, err := startMailpit(ctx)
	if err != nil {
		return 0, fmt.Errorf("стенд: Mailpit: %w", err)
	}
	box = mp
	defer box.stop(ctx)

	return m.Run(), nil
}
