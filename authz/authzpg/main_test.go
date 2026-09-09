package authzpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд
// общий с остальными адаптерами тулкита (postgres/pgtest): он же умеет
// TEST_DATABASE_URL, без которого мутационный прогон поднимал бы контейнер на
// каждого мутанта.
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

// newStore — схема на тест плюс накат из schema.sql: тестируется артефакт,
// который уедет в миграции потребителя, а не его копия в коде.
func newStore(t *testing.T) (*authzpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	return authzpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// testNow — момент так, как его хранит timestamptz: UTC и микросекунды.
func testNow() time.Time { return pgtest.Now() }

// staff — субъект витрины.
func staff(id string) authz.Subject { return authz.Subject{Realm: "staff", ID: id} }

// mustAssign — выдача, которая обязана удаться: подготовка данных теста.
func mustAssign(t *testing.T, s *authzpg.Store, a authzpg.Assignment) {
	t.Helper()
	require.NoError(t, s.Assign(t.Context(), a))
}

// grant — назначение с разумными умолчаниями.
func grant(sub authz.Subject, role authz.Role, mods ...func(*authzpg.Assignment)) authzpg.Assignment {
	a := authzpg.Assignment{
		Subject:   sub,
		Role:      role,
		GrantedBy: "admin-1",
		GrantedAt: testNow(),
	}
	for _, mod := range mods {
		mod(&a)
	}
	return a
}

// countRows — сколько назначений в таблице: проверки идут мимо адаптера.
func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM authz_role_assignments`).Scan(&n))
	return n
}
