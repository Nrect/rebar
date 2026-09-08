package pgtest_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь: пакет проверяет сам себя тем же способом,
// которым им пользуется потребитель.
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
