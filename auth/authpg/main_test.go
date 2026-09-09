package authpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/postgres/pgtest"
)

// testRealm — реалм тестов; форма та же, что в CHECK схемы.
const testRealm auth.Realm = "shop"

// otherRealm — второй реалм в каждом сценарии: реалм входит в ключ строки и в
// КАЖДЫЙ WHERE, и запрос, забывший его, обслужит чужие строки, с виду работая.
const otherRealm auth.Realm = "staff"

// db — база на весь тестовый бинарь; каждый тест заводит через неё свою схему,
// поэтому тесты идут параллельно и не видят строк друг друга.
var db *pgtest.DB

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	started, err := pgtest.Start(ctx, pgtest.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "старт Postgres:", err)
		os.Exit(1)
	}
	db = started
	code := m.Run()
	db.Close(context.Background())
	os.Exit(code)
}

// newStore — схема на тест плюс пул с search_path в неё; Up применяется из
// schema.sql, чтобы тестировался артефакт, а не его копия в коде.
func newStore(t *testing.T) (*authpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	return authpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// consumerUsersDDL — таблица пользователей ПОТРЕБИТЕЛЯ. Её в пакете нет и не
// будет; тестам она нужна, чтобы доказать атомарность эффекта токена: строка
// токена и правка чужой таблицы обязаны жить и умирать вместе.
const consumerUsersDDL = `CREATE TABLE consumer_users (
	id       UUID PRIMARY KEY,
	login    TEXT NOT NULL UNIQUE,
	verified BOOLEAN NOT NULL DEFAULT false
)`

func withConsumerUsers(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	pgtest.Apply(t, pool, consumerUsersDDL)
	id := uuid.New()
	_, err := pool.Exec(t.Context(), `INSERT INTO consumer_users (id, login) VALUES ($1, $2)`,
		id, "alice@example.invalid")
	require.NoError(t, err)
	return id
}

func verifiedFlag(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var verified bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT verified FROM consumer_users WHERE id = $1`, id).Scan(&verified))
	return verified
}

// testSession — сессия в том виде, в каком её отдаёт session.Service.
func testSession(now time.Time, mods ...func(*session.Session)) session.Session {
	s := session.Session{
		TokenHash:     uuid.New().String(),
		Realm:         testRealm,
		SubjectID:     uuid.New(),
		CreatedAt:     now,
		LastSeenAt:    now,
		ExpiresAt:     now.Add(24 * time.Hour),
		IdleExpiresAt: now.Add(2 * time.Hour),
		IP:            "203.0.113.7",
		UserAgent:     "rebar-test/1.0",
	}
	for _, mod := range mods {
		mod(&s)
	}
	return s
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), query, args...).Scan(&n))
	return n
}
