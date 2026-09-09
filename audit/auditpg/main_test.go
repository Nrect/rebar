package auditpg_test

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

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// secretName — «персональные данные» тестов: ищем их в текстах ошибок.
const secretName = "teacher-42@school.ru"

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд
// общий с остальными адаптерами тулкита (postgres/pgtest): он умеет
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
		fmt.Fprintln(os.Stderr, "старт Postgres:", err)
		os.Exit(1)
	}
	db = started
	code := m.Run()
	db.Close(ctx)
	os.Exit(code)
}

// newSink — схема на тест плюс накат из schema.sql: тестируется артефакт,
// который уедет в миграции потребителя, а не его копия в коде.
func newSink(t *testing.T) (*auditpg.Sink, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	return auditpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// testNow — момент так, как его хранит timestamptz: UTC и микросекунды.
func testNow() time.Time { return pgtest.Now() }

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
