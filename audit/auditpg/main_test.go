package auditpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
)

// postgresImage — digest-пин, как в mailpg.
const postgresImage = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

// secretName — «персональные данные» тестов: ищем их в текстах ошибок.
const secretName = "teacher-42@school.ru"

// maxPoolConns — потолок соединений на пул: сумма пулов параллельных тестов
// остаётся ниже max_connections Postgres.
const maxPoolConns = 5

// adminPool — общий пул к базе контейнера; каждый тест заводит через него свою
// схему, поэтому тесты идут параллельно и не видят строк друг друга.
var adminPool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	ctr, dsn, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "старт Postgres:", err)
		os.Exit(1)
	}
	if adminPool, err = newPool(ctx, dsn, ""); err != nil {
		fmt.Fprintln(os.Stderr, "пул к Postgres:", err)
		os.Exit(1)
	}
	code := m.Run()
	adminPool.Close()
	_ = ctr.Terminate(context.Background())
	os.Exit(code)
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "audit",
				"POSTGRES_PASSWORD": "audit",
				"POSTGRES_DB":       "audit",
			},
			WaitingFor: wait.ForAll(
				// Инициализация поднимает временный сервер и перезапускает его:
				// строка в логе появляется дважды, годится вторая.
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
	return ctr, fmt.Sprintf("postgres://audit:audit@%s:%d/audit", host, port.Num()), nil
}

// newSink — схема на тест плюс пул с search_path в неё; Up применяется из
// schema.sql, чтобы тестировался артефакт, а не его копия в коде.
func newSink(t *testing.T) (*auditpg.Sink, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	_, err := pool.Exec(context.Background(), schemaUp(t))
	require.NoError(t, err, "применение -- +goose Up из schema.sql")
	return auditpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker (Postgres)")
	}
	ctx := context.Background()
	schema := "t" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "создание схемы теста")

	pool, err := newPool(ctx, adminPool.Config().ConnString(), schema)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// newPool — пул к базе контейнера, при непустой schema — с search_path в неё.
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

// testEvent — событие в том виде, в каком его отдаёт audit.Recorder.Prepare.
func testEvent(mods ...func(*audit.Event)) audit.Event {
	ev := audit.Event{
		ID:        uuid.New(),
		At:        testNow(),
		Action:    "user.login",
		Outcome:   audit.OutcomeDenied,
		Actor:     audit.Actor{Kind: audit.ActorUser, ID: "u-1", Name: secretName},
		Target:    audit.Target{Type: "user", ID: "u-1"},
		RequestID: "req-1",
		IP:        "203.0.113.7",
		Details:   map[string]string{"reason": "bad_password"},
	}
	for _, mod := range mods {
		mod(&ev)
	}
	return ev
}

// eventRow — строка так, как её видит база: проверки идут мимо адаптера.
type eventRow struct {
	OccurredAt time.Time
	Action     string
	Outcome    string
	ActorKind  string
	ActorID    string
	ActorName  string
	TargetType string
	TargetID   string
	RequestID  string
	IP         string
	Details    map[string]string
}

func readRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) eventRow {
	t.Helper()
	var r eventRow
	err := pool.QueryRow(context.Background(), `SELECT occurred_at, action, outcome, actor_kind,
		actor_id, actor_name, target_type, target_id, request_id, ip, details
		FROM audit_events WHERE id = $1`, id).
		Scan(&r.OccurredAt, &r.Action, &r.Outcome, &r.ActorKind, &r.ActorID, &r.ActorName,
			&r.TargetType, &r.TargetID, &r.RequestID, &r.IP, &r.Details)
	require.NoError(t, err)
	return r
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}
