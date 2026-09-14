package pglock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь. Advisory lock общий на базу, поэтому
// каждый тест берёт своё имя задачи (jobName), а не свою схему.
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

// testBudget — потолок ожидания в тесте: сломанный прогон падает по нему, а не
// висит до таймаута всего бинаря.
const testBudget = 10 * time.Second

// replicaMaxConns — потолок пула реплики в тестах.
const replicaMaxConns = 5

// replica — пул отдельной реплики: свои соединения, база общая. Схема не
// нужна: advisory lock общий на базу.
func replica(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return newPool(t, nil)
}

// staleConnPool — пул на одно соединение без пинга при выдаче: мёртвая сессия
// достаётся прогону как есть, а не подменяется свежей.
func staleConnPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return newPool(t, func(cfg *pgxpool.Config) {
		cfg.MaxConns = 1
		cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	})
}

// newPool — пул к базе прогона, закрываемый РОВНО в одном месте и с бюджетом:
// соединение, не вернувшееся в пул, роняет тест, а не вешает бинарь. Второй
// Close (как Cleanup у pgtest.Schema) ждал бы первый внутри sync.Once, и бюджет
// не спас бы.
func newPool(t *testing.T, tune func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	cfg, err := pgxpool.ParseConfig(db.DSN())
	require.NoError(t, err)
	cfg.MaxConns = replicaMaxConns
	if tune != nil {
		tune(cfg)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			pool.Close()
		}()
		select {
		case <-closed:
		case <-time.After(testBudget):
			t.Error("пул не закрылся: соединение не вернулось в пул")
		}
	})
	return pool
}

// exhaust занимает все соединения пула до конца теста.
func exhaust(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for range pool.Config().MaxConns {
		conn, err := pool.Acquire(runCtx(t))
		require.NoError(t, err)
		t.Cleanup(conn.Release)
	}
}

// unreachablePool — пул к порту, где никто не слушает: база недоступна и без
// Docker.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://pglock@127.0.0.1:1/pglock?sslmode=disable&connect_timeout=2")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// jobName — своё имя задачи на тест: тесты идут параллельно в одной базе.
func jobName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "t" + hex.EncodeToString(b)
}

// runCtx — контекст прогона с бюджетом теста.
func runCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testBudget)
	t.Cleanup(cancel)
	return ctx
}

// waitFor ждёт закрытия канала не дольше бюджета.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testBudget):
		t.Fatalf("не дождались: %s", what)
	}
}

// receive — значение из канала не дольше бюджета.
func receive[T any](t *testing.T, ch <-chan T, what string) (v T) {
	t.Helper()
	select {
	case v = <-ch:
	case <-time.After(testBudget):
		t.Fatalf("не дождались: %s", what)
	}
	return v
}

// keyHalves — ключ так, как его показывает pg_locks: classid и objid.
func keyHalves(name string) (high, low int64) {
	key := uint64(Key(name))
	return int64(key >> 32), int64(key & 0xffffffff)
}

// holders — сколько сессий этой базы держат ключ задачи.
func holders(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	high, low := keyHalves(name)
	var n int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND classid::int8 = $1 AND objid::int8 = $2`, high, low).Scan(&n)
	require.NoError(t, err)
	return n
}

// waitReleased ждёт, пока Postgres снимет ключ умершей сессии: бэкенд
// завершается уже после разрыва, асинхронно.
func waitReleased(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	deadline := time.Now().Add(testBudget)
	for holders(t, pool, name) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("ключ %s не снялся за %s", name, testBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdKey — ключ задачи в чужой сессии, мимо Locker: так тест изображает
// реплику-соседа. Соединение закрывается и в Cleanup.
func holdKey(t *testing.T, name string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), db.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var took bool
	require.NoError(t, conn.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", Key(name)).Scan(&took))
	require.True(t, took, "ключ свежей задачи обязан быть свободен")
	return conn
}

// recorder — Observer теста: исходы по задачам в порядке прихода.
type recorder struct {
	mu      sync.Mutex
	results map[string][]Result
}

func newRecorder() *recorder { return &recorder{results: map[string][]Result{}} }

func (r *recorder) Outcome(_ context.Context, job string, result Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results[job] = append(r.results[job], result)
}

func (r *recorder) of(job string) []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.results[job])
}

// panicking — Observer, который падает на любом исходе.
type panicking struct{}

func (panicking) Outcome(context.Context, string, Result) { panic("наблюдатель упал") }
