package idempg_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// raceBudget — потолок ожидания там, где реализация обязана ответить сразу:
// сломанный прогон падает по нему, а не висит до таймаута бинаря.
const raceBudget = 10 * time.Second

// lazyPool — пул, который не соединяется, пока его не позовут: конструктору
// нужен настоящий *pgxpool.Pool, а базе здесь делать нечего.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://idempg@127.0.0.1:1/none")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// widePool — пул в ту же схему с потолком соединений n: гонке нужно соединение
// на каждого участника, иначе они ждут пул, а не ключ.
func widePool(t *testing.T, pool *pgxpool.Pool, n int32) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.MaxConns = n
	wide, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(wide.Close)
	return wide
}

// Fail closed: негодная сборка приложения падает на старте, а не на первом
// запросе. Тексты — как у idemtest.NewMemStore: тот же конструктор.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	pool := lazyPool(t)
	obs := idemtest.NewObserver()
	assert.PanicsWithValue(t, "idempg.New: nil pool", func() { idempg.New(nil, testConfig(), obs) })
	assert.PanicsWithValue(t, "idempg.New: observer must not be nil", func() { idempg.New(pool, testConfig(), nil) })
	bad := testConfig()
	bad.Retention = time.Hour
	assert.PanicsWithValue(t, "idempg.New: Config.Retention must be at least 24h0m0s, got 1h0m0s",
		func() { idempg.New(pool, bad, obs) })
	assert.PanicsWithValue(t, "idempg.New: Config.Operations must list at least one operation",
		func() { idempg.New(pool, idem.Config{}, obs) })

	store := idempg.New(pool, testConfig(), obs)
	assert.PanicsWithValue(t, "idempg.WithTx: nil tx", func() { store.WithTx(nil) })
	assert.PanicsWithValue(t, "idempg.Store.SetClock: now must not be nil", func() { store.SetClock(nil) })
	assert.PanicsWithValue(t, "idempg.Store.Do: op must not be nil",
		func() { _, _ = store.Do(t.Context(), request(t, "nil-op"), nil) })
}

// То, что адаптер решает до базы, базы не трогает: запрос, собранный неверно,
// и непозитивный limit уборки отвечают и на недоступном пуле, и по отменённому
// контексту. Контроль — тот же пул на законном вызове отвечает сбоем.
func TestStore_RefusalsBeforeDatabase(t *testing.T) {
	t.Parallel()

	obs := idemtest.NewObserver()
	store := idempg.New(lazyPool(t), testConfig(), obs)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	invalid := request(t, "before-db")
	invalid.Scope = idem.Scope{}
	_, err := store.Do(cancelled, invalid, notRun)
	require.ErrorIs(t, err, idem.ErrInvalidScope)
	for _, limit := range []int{0, -1} {
		n, purgeErr := store.Purge(cancelled, testNow, limit)
		require.NoError(t, purgeErr, "limit %d", limit)
		assert.Zero(t, n, "limit %d", limit)
	}
	assert.Empty(t, obs.Outcomes(), "отказ до базы наблюдателю не отдаётся")

	_, err = store.Purge(t.Context(), testNow, 1)
	require.ErrorIs(t, err, idem.ErrUnavailable, "контроль: пул и правда недоступен")
	_, err = store.Do(t.Context(), request(t, "before-db"), notRun)
	require.ErrorIs(t, err, idem.ErrUnavailable, "контроль: законный запрос идёт в базу")
	assert.Equal(t, []idem.Outcome{idem.OutcomeError}, outcomesOf(obs))
}

// refuseCommit — отложенный триггер, роняющий COMMIT транзакции, в которой лёг
// заказ refuse-commit. Прежде чем отказать, он проверяет, что запись ответа уже
// в транзакции: отказ приходит после всех шагов Do, а не вместо них.
func refuseCommit(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	pgtest.Apply(t, pool, `
CREATE FUNCTION shop_orders_refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM idem_records) THEN
		RAISE EXCEPTION 'idempg_test: к фиксации записи нет';
	END IF;
	RAISE EXCEPTION 'idempg_test: фиксация отказала';
END $$;
CREATE CONSTRAINT TRIGGER shop_orders_refuse_commit_trg AFTER INSERT ON shop_orders
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.label = 'refuse-commit')
	EXECUTE FUNCTION shop_orders_refuse_commit();`)
}

// Решение 2.1: ответ и эффект — одна транзакция. После каждого отказа из базы
// читаются ОБЕ стороны — заказ и запись, — и ключ свободен: законный повтор
// исполняет op заново, а не получает отказ или чужой ответ.
func TestStore_Do_IsAtomic(t *testing.T) {
	t.Parallel()

	serverError := created(1)
	serverError.Status = http.StatusServiceUnavailable
	tooLarge := created(1)
	tooLarge.Body = []byte(strings.Repeat("x", maxResponse))

	refusals := []struct {
		name  string
		label string
		op    func(context.Context, pgx.Tx) (idem.Response, error)
		want  error
	}{
		{"ошибка op", "op-error", failedOrder("op-error", errOp), errOp},
		{"ответ 5xx", "server-error", order("server-error", serverError), idem.ErrNotRecordable},
		{"ответ сверх потолка", "too-large", order("too-large", tooLarge), idem.ErrResponseTooLarge},
		{"сбой фиксации", "refuse-commit", order("refuse-commit", created(1)), idem.ErrUnavailable},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, pool, _ := newStore(t)
			refuseCommit(t, pool)
			req := request(t, tc.label)

			_, err := store.Do(t.Context(), req, tc.op)
			require.ErrorIs(t, err, tc.want)
			if tc.label == "refuse-commit" {
				require.ErrorContains(t, err, "idempg_test: фиксация отказала", "отказ пришёл на COMMIT, после записи")
			}
			assert.Zero(t, orders(t, pool), "эффект пережил откат")
			assert.Zero(t, records(t, pool), "запись пережила откат")

			res, err := store.Do(t.Context(), req, order("retry", created(2)))
			require.NoError(t, err)
			assert.False(t, res.Replayed, "повтор после отказа исполняется заново")
			assert.Equal(t, [2]int{1, 1}, [2]int{orders(t, pool), records(t, pool)})
		})
	}

	// CORRECTNESS §7: запись, вставленная отдельным соединением, пережила бы
	// откат транзакции потребителя и отвергла бы его законный повтор.
	t.Run("откат транзакции потребителя", func(t *testing.T) {
		t.Parallel()
		store, pool, _ := newStore(t)
		req := request(t, "consumer-rollback")

		tx := beginTx(t, pool)
		res, err := store.WithTx(tx).Do(t.Context(), req, order("rolled-back", created(1)))
		require.NoError(t, err)
		require.False(t, res.Replayed)
		require.NoError(t, tx.Rollback(t.Context()))
		assert.Zero(t, orders(t, pool), "эффект пережил откат потребителя")
		assert.Zero(t, records(t, pool), "запись пережила откат потребителя")

		tx = beginTx(t, pool)
		res, err = store.WithTx(tx).Do(t.Context(), req, order("committed", created(2)))
		require.NoError(t, err)
		assert.False(t, res.Replayed, "ключ свободен: законный повтор исполняется")
		require.NoError(t, tx.Commit(t.Context()))

		res, err = store.Do(t.Context(), req, notRun)
		require.NoError(t, err)
		assert.True(t, res.Replayed)
		assert.Equal(t, created(2), res.Response)
		assert.Equal(t, [2]int{1, 1}, [2]int{orders(t, pool), records(t, pool)})
	})
}

// Решение 2.2: N параллельных Do с одним ключом — эффект в базе ровно один.
// ПАРАЛЛЕЛЬНЫЙ ПОВТОР НЕ ЖДЁТ: op держит ключ, пока остальные не вернутся, и
// реализация, ждущая на ключе или на уникальном индексе, упирается в потолок, а
// не проходит тест случайным порядком.
func TestStore_Do_Race(t *testing.T) {
	t.Parallel()

	const attempts = 12
	schema := newSchemaPool(t)
	applyUp(t, schema)
	createOrders(t, schema)
	pool := widePool(t, schema, attempts+2)
	obs := idemtest.NewObserver()
	store := idempg.New(pool, testConfig(), obs)
	req := request(t, "race")

	var (
		calls  atomic.Int32
		waited atomic.Bool
	)
	returned := make(chan struct{}, attempts)
	op := func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
		n := calls.Add(1)
		if _, err := tx.Exec(ctx, `INSERT INTO shop_orders (label) VALUES ($1)`, fmt.Sprintf("race-%d", n)); err != nil {
			return idem.Response{}, err
		}
		deadline := time.After(raceBudget)
		for range attempts - 1 {
			select {
			case <-returned:
			case <-deadline:
				waited.Store(true)
				return created(1), nil
			}
		}
		return created(1), nil
	}

	type outcome struct {
		res idem.Result
		err error
	}
	results := make(chan outcome, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			<-start
			res, err := store.Do(t.Context(), req, op)
			results <- outcome{res: res, err: err}
			returned <- struct{}{}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	require.False(t, waited.Load(), "op дождалась потолка: параллельные Do ждали на ключе вместо in_flight")
	executed, inFlight := 0, 0
	for r := range results {
		switch {
		case r.err == nil && !r.res.Replayed:
			executed++
			assert.Equal(t, created(1), r.res.Response)
		case errors.Is(r.err, idem.ErrInFlight):
			inFlight++
		default:
			t.Errorf("параллельный Do: исход %+v, ошибка %v", r.res, r.err)
		}
	}
	assert.Equal(t, 1, executed, "исполненных Do")
	assert.Equal(t, attempts-1, inFlight, "пока op держит ключ, остальные — in_flight")
	assert.Equal(t, int32(1), calls.Load(), "исполнений op")
	assert.Equal(t, 1, orders(t, pool), "эффект в базе ровно один")
	assert.Equal(t, 1, records(t, pool), "запись ровно одна")
	assert.Len(t, obs.Outcomes(), attempts, "исход на каждый Do")
}

// Эталон ключа блокировки, посчитанный вне Go (TestLockKey_GoldenVector), —
// ровно тот ключ, который Do держит во время op: он виден в pg_locks у
// соединения Do под classid и objid старших и младших 32 бит.
func TestStore_Do_LocksGoldenKey(t *testing.T) {
	t.Parallel()

	golden := uint64(0xf4d88f592409e8fb) // -803734920466142981: ("customers", "7d9c3f1e-…-0a1b2c3d4e5f", "k-1")
	store, pool, _ := newStore(t)
	req := request(t, "k-1")
	req.Scope.Subject = "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"

	held := -1
	_, err := store.Do(t.Context(), req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return idem.Response{}, err
		}
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND objsubid = 1 AND classid = $1 AND objid = $2 AND pid = $3`,
			uint32(golden>>32), uint32(golden&0xffffffff), pid).Scan(&held)
		return created(1), err
	})
	require.NoError(t, err)
	assert.Equal(t, 1, held, "во время op соединение Do держит эталонный ключ")
}

// Момент записи уходит в базу как timestamptz: часы в чужом поясе с
// наносекундами дают в колонке то же мгновение, усечённое до микросекунд
// (CONVENTIONS §11). Прочитанное приводится к UTC сразу после Scan.
func TestStore_Do_WritesMomentAsTimestamptz(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	at := time.Date(2026, 9, 16, 23, 30, 0, 123456789, time.FixedZone("UTC+05:45", 5*60*60+45*60))
	store.SetClock(func() time.Time { return at })
	req := request(t, "moment")
	_, err := store.Do(t.Context(), req, order("moment", created(1)))
	require.NoError(t, err)

	var stored time.Time
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT created_at FROM idem_records WHERE subject = $1`,
		req.Scope.Subject).Scan(&stored))
	stored = stored.UTC()
	want := time.Date(2026, 9, 16, 17, 45, 0, 123456000, time.UTC)
	assert.True(t, want.Equal(stored), "в колонке %v, ожидалось %v", stored, want)
	assert.Equal(t, time.UTC, stored.Location())
}

// insertRecord — запись мимо адаптера и мимо блокировки ключа, отдельной
// транзакцией: так пишет раннер без блокировки или реплика с другой формулой.
func insertRecord(t *testing.T, pool *pgxpool.Pool, req idem.Request, resp idem.Response, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO idem_records
		(realm, subject, idem_key, operation, fingerprint, status, content_type, location, body, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		req.Scope.Realm, req.Scope.Subject, req.Key.String(), string(req.Operation), req.Fingerprint(),
		resp.Status, resp.ContentType, resp.Location, resp.Body, at)
	require.NoError(t, err)
}

// Вставка под взятой блокировкой упёрлась в запись, легшую мимо неё, — в пуле
// решает перечитанная запись: тот же запрос получает её ответ, другой —
// ErrKeyReused, а эффект op уходит откатом. (В WithTx это отказ —
// TestStore_WithTx_AbortsOnError.)
func TestStore_Do_RecordWrittenPastLock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		change  func(r *idem.Request)
		want    error
		outcome idem.Outcome
	}{
		{"тот же запрос — ответ записи", func(*idem.Request) {}, nil, idem.OutcomeReplayed},
		{"другой запрос — ErrKeyReused", func(r *idem.Request) { r.Body = []byte(`{"product":"b"}`) },
			idem.ErrKeyReused, idem.OutcomeReused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, pool, obs := newStore(t)
			req := request(t, "past-lock")
			foreign := req
			tc.change(&foreign)

			res, err := store.Do(t.Context(), req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
				resp, err := order("past-lock", created(1))(ctx, tx)
				insertRecord(t, pool, foreign, created(7), testNow)
				return resp, err
			})
			require.ErrorIs(t, err, tc.want)
			if tc.want == nil {
				assert.True(t, res.Replayed)
				assert.Equal(t, created(7), res.Response, "ответ записи, легшей первой")
			}
			assert.Zero(t, orders(t, pool), "эффект op ушёл откатом")
			assert.Equal(t, 1, records(t, pool))
			assert.Equal(t, []idem.Outcome{tc.outcome}, outcomesOf(obs))
		})
	}
}

// Уборка — самые старые первыми, равные моменты — по ключу побайтно, как у
// двойника, а не по локали базы. Записи ложатся в порядке, обратном удалению:
// физический порядок строк не подсказывает ответ. Граница — строго раньше, до
// микросекунды.
func TestStore_Purge_OldestFirst(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	// Стенд на musl сортирует en_US побайтно; правило ICU — как у базы на glibc.
	pgtest.Apply(t, pool, `ALTER TABLE idem_records ALTER COLUMN idem_key TYPE text COLLATE "und-x-icu"`)
	at := time.Date(2026, 9, 1, 10, 0, 0, 123456000, time.UTC)
	scope := request(t, "later")
	// «B» раньше «a» побайтно и позже по правилам локали.
	for _, rec := range []struct {
		key string
		at  time.Time
	}{{"later", at.Add(time.Microsecond)}, {"a", at}, {"B", at}} {
		req := scope
		key, err := idem.ParseKey(rec.key)
		require.NoError(t, err)
		req.Key = key
		insertRecord(t, pool, req, created(1), rec.at)
	}

	purge := func(before time.Time, limit int) int {
		t.Helper()
		n, err := store.Purge(t.Context(), before, limit)
		require.NoError(t, err)
		return n
	}
	assert.Zero(t, purge(at, 10), "момент записи на границе не удаляется")
	assert.Zero(t, purge(at.Add(900*time.Nanosecond), 10), "граница внутри той же микросекунды")

	boundary := at.Add(2 * time.Microsecond)
	for _, want := range [][]string{{"a", "later"}, {"later"}, {}} {
		assert.Equal(t, 1, purge(boundary, 1), "пачка из одной записи")
		assert.Equal(t, want, keys(t, pool), "после пачки")
	}
	assert.Zero(t, purge(boundary, 1), "уборка после всех старых")
}

// keys — ключи записей по порядку ключа.
func keys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT idem_key FROM idem_records ORDER BY idem_key COLLATE "C"`)
	require.NoError(t, err)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return got
}

// mutation — запрос, переписывающий запись.
var mutation = regexp.MustCompile(`(?i)\bUPDATE\b`)

// Запись не переписывается: в строковых литералах адаптера нет UPDATE — ни
// голого, ни в ON CONFLICT DO UPDATE (решение 15). Литералы, а не текст файла:
// комментарии запрет как раз объясняют.
func TestAdapter_HasNoUpdate(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	queries := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			if strings.Contains(value, "idem_records") {
				queries++
			}
			assert.False(t, mutation.MatchString(value), "%s:%d: запрос адаптера переписывает запись: %q",
				name, fset.Position(lit.Pos()).Line, value)
			return true
		})
	}
	require.GreaterOrEqual(t, queries, 3, "запросов к idem_records не нашлось — тест смотрит не туда")
}
