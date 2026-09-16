package inboxpg_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд общий
// с остальными адаптерами тулкита: он умеет TEST_DATABASE_URL, без которого
// мутационный прогон поднимал бы контейнер на каждого мутанта.
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

// Источник и тип тестов.
const (
	billing  inbox.SourceName = "billing"
	typePaid inbox.EventType  = "invoice.paid"
)

// testNow — момент приёма так, как его хранит timestamptz.
var testNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// handlerFunc — функция как inboxpg.Handler.
type handlerFunc func(ctx context.Context, tx pgx.Tx, ev inbox.Event) error

func (f handlerFunc) Handle(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
	return f(ctx, tx, ev)
}

// nop — обработчик без эффекта.
var nop = handlerFunc(func(context.Context, pgx.Tx, inbox.Event) error { return nil })

// only — обработчики единственного источника тестов.
func only(h inboxpg.Handler) map[inbox.SourceName]inboxpg.Handler {
	return map[inbox.SourceName]inboxpg.Handler{billing: h}
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// newStore — схема на тест, накат каталога миграций и адаптер над ней.
// Накатывается сам каталог, а не его копия в коде: тестируется артефакт, который
// уедет раннеру потребителя (applyUp — pgtestcopy_test.go).
func newStore(t *testing.T, handlers map[inbox.SourceName]inboxpg.Handler) (*inboxpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	applyUp(t, pool)
	return inboxpg.New(pool, handlers), pool
}

// testEvent — событие так, как его отдаёт ядро: отпечаток по телу, момент до приёма.
func testEvent(id, payload string) inbox.Event {
	sum := sha256.Sum256([]byte(payload))
	return inbox.Event{
		Source: billing, ID: id, Type: typePaid,
		OccurredAt: testNow.Add(-time.Minute), Payload: []byte(payload), Digest: sum[:],
	}
}

// mustAccept — доставка без ошибки с ожидаемым исходом.
func mustAccept(t *testing.T, store *inboxpg.Store, ev inbox.Event, want inbox.Outcome) {
	t.Helper()
	outcome, err := store.Accept(t.Context(), ev, testNow)
	require.NoError(t, err, "доставка %s", ev.ID)
	require.Equal(t, want, outcome, "исход доставки %s", ev.ID)
}

// shopTables — таблицы потребителя: эффект обработчика и его бизнес-факт. Без
// уникальности: двойной эффект виден счётом строк.
func shopTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `CREATE TABLE shop_effects (event_id TEXT NOT NULL);
		CREATE TABLE shop_orders (id TEXT NOT NULL)`)
	require.NoError(t, err)
}

// recordEffect — обработчик, пишущий эффект транзакцией приёма.
func recordEffect(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
	_, err := tx.Exec(ctx, `INSERT INTO shop_effects (event_id) VALUES ($1)`, ev.ID)
	return err
}

// beginTx — транзакция потребителя. Откат сразу в Cleanup: тест, упавший до
// своего Rollback, иначе вешал бы pool.Close на занятом соединении до таймаута.
func beginTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// stored — строк отметки, тела и эффекта события.
func stored(t *testing.T, pool *pgxpool.Pool, id string) [3]int {
	t.Helper()
	var n [3]int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM inbox_events WHERE event_id = $1),
		(SELECT count(*) FROM inbox_payloads WHERE event_id = $1),
		(SELECT count(*) FROM shop_effects WHERE event_id = $1)`, id).Scan(&n[0], &n[1], &n[2]))
	return n
}

// reader — окно набора в базу мимо порта: запросы теста, а не код адаптера
// (уточнение арбитра 20).
type reader struct{ pool *pgxpool.Pool }

var _ inboxtest.Reader = reader{}

func (r reader) Mark(ctx context.Context, source inbox.SourceName, id string) (inboxtest.Mark, bool, error) {
	var m inboxtest.Mark
	err := r.pool.QueryRow(ctx, `SELECT event_type, digest, occurred_at, received_at FROM inbox_events
		WHERE source = $1 AND event_id = $2`, source, id).Scan(&m.Type, &m.Digest, &m.OccurredAt, &m.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return inboxtest.Mark{}, false, nil
	}
	if err != nil {
		return inboxtest.Mark{}, false, err
	}
	// pgx отдаёт timestamptz в местной зоне; значение — ровно то, что хранит база.
	m.OccurredAt, m.ReceivedAt = m.OccurredAt.UTC(), m.ReceivedAt.UTC()
	return m, true, nil
}

func (r reader) Payload(ctx context.Context, source inbox.SourceName, id string) (payload []byte, ok bool, err error) {
	err = r.pool.QueryRow(ctx, `SELECT payload FROM inbox_payloads WHERE source = $1 AND event_id = $2`,
		source, id).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

// queryErrors — трассировщик pgx: последняя ошибка запроса на соединении, как её
// увидит трассировщик потребителя.
type queryErrors struct {
	mu   sync.Mutex
	last map[*pgx.Conn]error
}

func (q *queryErrors) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (q *queryErrors) TraceQueryEnd(_ context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.last[conn] = data.Err
}

func (q *queryErrors) lastOn(conn *pgx.Conn) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.last[conn]
}

// forget — соединение пришло из пула в новую транзакцию: ошибка прошлой ему не причина.
func (q *queryErrors) forget(conn *pgx.Conn) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.last, conn)
}

// newTracedStore — то же, что newStore, но пул со своим трассировщиком запросов.
func newTracedStore(t *testing.T, handlers map[inbox.SourceName]inboxpg.Handler,
) (*inboxpg.Store, *pgxpool.Pool, *queryErrors) {
	t.Helper()
	pgtest.Short(t)
	cfg, err := pgxpool.ParseConfig(pgtest.SchemaDSN(t, db))
	require.NoError(t, err)
	traced := &queryErrors{last: map[*pgx.Conn]error{}}
	cfg.ConnConfig.Tracer = traced
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	applyUp(t, pool)
	return inboxpg.New(pool, handlers), pool, traced
}
