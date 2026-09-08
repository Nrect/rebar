package authzpg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
)

// postgresImage — digest-пин, как в остальных адаптерах тулкита.
const postgresImage = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

// envDatabaseURL — сервер, на котором гонять тесты. Пусто — поднимается
// контейнер. Задан — каждый тест заводит свою схему на этом сервере: иначе
// мутационный прогон поднимал бы контейнер на каждого мутанта и врал
// таймаутами (docs/CHIP.md).
const envDatabaseURL = "TEST_DATABASE_URL"

// maxPoolConns — потолок соединений на пул; сумма пулов параллельных тестов
// должна оставаться ниже max_connections сервера.
const maxPoolConns = 4

// adminPool — общий пул; каждый тест заводит через него свою схему, поэтому
// тесты идут параллельно и не видят строк друг друга.
var adminPool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	dsn, stop, err := server(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "база для тестов:", err)
		os.Exit(1)
	}
	if adminPool, err = newPool(ctx, dsn, ""); err != nil {
		stop()
		fmt.Fprintln(os.Stderr, "пул к Postgres:", err)
		os.Exit(1)
	}
	code := m.Run()
	adminPool.Close()
	stop()
	os.Exit(code)
}

// server — сервер для прогона: заданный переменной окружения либо контейнер.
func server(ctx context.Context) (dsn string, stop func(), err error) {
	if external := os.Getenv(envDatabaseURL); external != "" {
		return external, func() {}, nil
	}
	ctr, containerDSN, err := startPostgres(ctx)
	if err != nil {
		return "", func() {}, err
	}
	return containerDSN, func() { _ = ctr.Terminate(context.Background()) }, nil
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "authz",
				"POSTGRES_PASSWORD": "authz",
				"POSTGRES_DB":       "authz",
			},
			WaitingFor: wait.ForAll(
				// Инициализация поднимает временный сервер и перезапускает
				// его: строка в логе появляется дважды, годится вторая.
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeoutDefault(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, "", err
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, "", err
	}
	port, err := ctr.MappedPort(ctx, "5432")
	if err != nil {
		return nil, "", err
	}
	return ctr, fmt.Sprintf("postgres://authz:authz@%s:%d/authz", host, port.Num()), nil
}

// newStore — схема на тест плюс пул с search_path в неё; Up применяется из
// schema.sql, чтобы тестировался артефакт, а не его копия в коде.
func newStore(t *testing.T) (*authzpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	_, err := pool.Exec(t.Context(), schemaUp(t))
	require.NoError(t, err, "применение -- +goose Up из schema.sql")
	return authzpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker или " + envDatabaseURL)
	}
	ctx := context.Background()
	schema := "t" + randomHex(t)
	_, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "создание схемы теста")
	t.Cleanup(func() {
		// Схему сносим за собой: на внешнем сервере она иначе копится.
		_, _ = adminPool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	pool, err := newPool(ctx, adminPool.Config().ConnString(), schema)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func randomHex(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return hex.EncodeToString(buf)
}

// newPool — пул к базе, при непустой schema — с search_path в неё.
func newPool(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxPoolConns
	cfg.MinConns = 0
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// schemaSQL — файл читается один раз на пакет.
var schemaSQL = sync.OnceValues(func() (string, error) {
	raw, err := os.ReadFile("schema.sql")
	return string(raw), err
})

func schemaUp(t *testing.T) string {
	t.Helper()
	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok, "в schema.sql нет маркера %s", gooseUp)
	return up
}

// testNow — микросекунды: timestamptz хранит их, наносекунды Go теряет, и
// сравнение прочитанного времени с исходным иначе всегда красное.
func testNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

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
