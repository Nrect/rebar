package idempg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
	"github.com/nrect/rebar/kit/errs"
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

// Данные тестов: две операции и маленький потолок ответа.
const (
	opCreate    = idem.Operation("orders.create")
	opUpdate    = idem.Operation("orders.update")
	maxResponse = 256
	jsonType    = "application/json"
	testRealm   = "customers"
)

// testNow — часы записи: настоящее время тестам не нужно.
var testNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// errOp — отказ op, который обязан дойти до вызывающего как есть.
var errOp = errs.Conflict("seat-taken")

func testConfig() idem.Config {
	return idem.Config{
		Operations:       []idem.Operation{opCreate, opUpdate},
		Retention:        idem.MinRetention,
		MaxResponseBytes: maxResponse,
	}
}

// newSchemaPool — пул в пустую схему теста: миграции ещё не применены.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// newStore — схема на тест, накат каталога миграций, таблица эффекта op и
// адаптер над ними. Накатывается сам каталог, а не его копия в коде:
// тестируется артефакт, который уедет раннеру потребителя.
func newStore(t *testing.T) (*idempg.Store, *pgxpool.Pool, *idemtest.Observer) {
	t.Helper()
	pool := newSchemaPool(t)
	applyUp(t, pool)
	createOrders(t, pool)
	obs := idemtest.NewObserver()
	store := idempg.New(pool, testConfig(), obs)
	store.SetClock(idemtest.NewClock(testNow).Now)
	return store, pool, obs
}

// createOrders — таблица потребителя: эффект op в транзакции Do.
func createOrders(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	pgtest.Apply(t, pool, `CREATE TABLE shop_orders (label TEXT PRIMARY KEY)`)
}

// order — op, которая пишет заказ label через транзакцию Do и отвечает resp.
func order(label string, resp idem.Response) func(context.Context, pgx.Tx) (idem.Response, error) {
	return func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
		if _, err := tx.Exec(ctx, `INSERT INTO shop_orders (label) VALUES ($1)`, label); err != nil {
			return idem.Response{}, err
		}
		return resp, nil
	}
}

// failedOrder — op, которая пишет заказ и отказывает err: запись заказа обязана
// уйти вместе с отказом.
func failedOrder(label string, err error) func(context.Context, pgx.Tx) (idem.Response, error) {
	return func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
		if _, execErr := tx.Exec(ctx, `INSERT INTO shop_orders (label) VALUES ($1)`, label); execErr != nil {
			return idem.Response{}, execErr
		}
		return idem.Response{}, err
	}
}

// request — POST /orders под ключом key в своей области: параллельные тесты
// не делят ни записей, ни блокировок.
func request(t *testing.T, key string) idem.Request {
	t.Helper()
	parsed, err := idem.ParseKey(key)
	require.NoError(t, err)
	return idem.Request{
		Scope: idem.Scope{Realm: testRealm, Subject: randomSubject(t)}, Operation: opCreate, Key: parsed,
		Method: http.MethodPost, Path: "/orders", RawQuery: "source=web", Body: []byte(`{"product":"a"}`),
	}
}

func randomSubject(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// created — ответ «заказ n создан».
func created(n int) idem.Response {
	return idem.Response{
		Status: http.StatusCreated, ContentType: jsonType,
		Location: fmt.Sprintf("/orders/%d", n), Body: fmt.Appendf(nil, `{"order":%d}`, n),
	}
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

// orders и records — строк эффекта и записей в базе, мимо адаптера.
func orders(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM shop_orders`)
}

func records(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM idem_records`)
}

// outcomesOf — исходы наблюдателя по порядку.
func outcomesOf(obs *idemtest.Observer) []idem.Outcome {
	observed := obs.Outcomes()
	out := make([]idem.Outcome, 0, len(observed))
	for _, o := range observed {
		out = append(out, o.Outcome)
	}
	return out
}

// errNotRun — op, которая не должна была исполниться.
var errNotRun = errors.New("idempg_test: op must not run")

func notRun(context.Context, pgx.Tx) (idem.Response, error) { return idem.Response{}, errNotRun }
