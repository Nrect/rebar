package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

func TestNew_PanicsOnNilPool(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "postgres.New: nil pool", func() { postgres.New(nil, testConfig()) })
}

func TestRunner_InTx_Commits(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())

	err := run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `INSERT INTO counter (id, n) VALUES (1, 42)`)
		return execErr
	})

	require.NoError(t, err)
	assert.Equal(t, 1, countRows(t, pool))
}

func TestRunner_InTx_RollsBackOnError(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())
	sentinel := errors.New("бизнес-правило не выполнено")

	err := run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if _, execErr := tx.Exec(ctx, `INSERT INTO counter (id, n) VALUES (1, 42)`); execErr != nil {
			return execErr
		}
		return sentinel
	})

	require.ErrorIs(t, err, sentinel, "причина отката обязана дойти до вызывающего")
	assert.Zero(t, countRows(t, pool))
}

// Паника в fn не должна оставлять транзакцию открытой: соединение уехало бы в
// пул с блокировками до конца своей жизни.
func TestRunner_InTx_PanicRollsBackAndRepanics(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())

	assert.PanicsWithValue(t, "ошибка программиста", func() {
		_ = run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `INSERT INTO counter (id, n) VALUES (1, 42)`)
			panic("ошибка программиста")
		})
	})

	assert.Zero(t, countRows(t, pool), "паника не откатила транзакцию")
	// Пул жив: соединение вернулось пригодным, а не с открытой транзакцией.
	require.NoError(t, run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `INSERT INTO counter (id, n) VALUES (2, 1)`)
		return execErr
	}))
	assert.Equal(t, 1, countRows(t, pool))
}

func TestRunner_InTx_CancelledContextRollsBack(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())

	ctx, cancel := context.WithCancel(t.Context())
	err := run.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, execErr := tx.Exec(ctx, `INSERT INTO counter (id, n) VALUES (1, 42)`); execErr != nil {
			return execErr
		}
		cancel()
		return nil
	})

	require.Error(t, err, "commit по отменённому ctx не должен пройти")
	cancel()
	assert.Zero(t, countRows(t, pool))
}

// Таймауты стоят внутри транзакции и не протекают в пул: следующий, кому
// достанется это соединение, не унаследует чужие 250 мс.
func TestRunner_InTx_TimeoutsAreLocal(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	// Пул на одно соединение: иначе «после транзакции» проверялось бы на
	// другом соединении и тест был бы зелёным при SET вместо SET LOCAL.
	cfg, err := pgxpool.ParseConfig(db.DSN())
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	run := postgres.New(pool, postgres.Config{
		LockTimeout: 250 * time.Millisecond, StatementTimeout: 2500 * time.Millisecond,
		MaxAttempts: 1, RetryBase: time.Millisecond,
	})

	var inside [2]string
	require.NoError(t, run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).
			Scan(&inside[0], &inside[1])
	}))
	assert.Equal(t, "250ms", inside[0])
	assert.Equal(t, "2500ms", inside[1])

	var after [2]string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&after[0], &after[1]))
	assert.Equal(t, "0", after[0], "lock_timeout протёк в пул — это SET, а не SET LOCAL")
	assert.Equal(t, "0", after[1], "statement_timeout протёк в пул")
}

// Ошибка Postgres наружу приходит без Detail — а в Detail лежала вся строка.
func TestRunner_InTx_SanitizesError(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())
	pgtest.Apply(t, pool, `CREATE UNIQUE INDEX ux_counter_note ON counter (note)`)

	insert := func(id int) error {
		return run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `INSERT INTO counter (id, note) VALUES ($1, $2)`, id, secret)
			return execErr
		})
	}
	require.NoError(t, insert(1))

	err := insert(2)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret, "Detail с содержимым строки дошёл до вызывающего")
	assert.NotContains(t, err.Error(), "Failing row")
	assert.True(t, postgres.IsUniqueViolation(err, "ux_counter_note"), "классификация обязана пережить Sanitize")
	assert.False(t, postgres.IsUniqueViolation(err, "counter_pkey"), "чужой UNIQUE не повтор по ключу")
	assert.Equal(t, 1, countRows(t, pool))
}

// Дедлок двух встречных транзакций: Postgres убивает одну из них, InTxRetry
// проводит её со второй попытки.
func TestRunner_InTxRetry_SurvivesDeadlock(t *testing.T) {
	t.Parallel()
	run, pool := newRunner(t, testConfig())
	_, err := pool.Exec(t.Context(), `INSERT INTO counter (id, n) VALUES (1, 0), (2, 0)`)
	require.NoError(t, err)

	// Порядок обновления встречный — классический дедлок; барьер гарантирует,
	// что обе транзакции возьмут по первой строке до попытки взять вторую.
	var barrier sync.WaitGroup
	barrier.Add(2)
	bump := func(first, second int) error {
		once := true
		return run.InTxRetry(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
			for _, id := range []int{first, second} {
				if _, execErr := tx.Exec(ctx, `UPDATE counter SET n = n + 1 WHERE id = $1`, id); execErr != nil {
					return execErr
				}
				if once {
					once = false
					barrier.Done()
					barrier.Wait()
				}
			}
			return nil
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = bump(1, 2) }()
	go func() { defer wg.Done(); errs[1] = bump(2, 1) }()
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	var total int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT sum(n) FROM counter`).Scan(&total))
	assert.Equal(t, 4, total, "обе транзакции обязаны доехать целиком")
}

// Ожидание блокировки дольше lock_timeout — это 55P03: ждать можно, повторять
// незачем.
func TestRunner_LockTimeout_IsContention(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.LockTimeout = 100 * time.Millisecond
	run, pool := newRunner(t, cfg)
	_, err := pool.Exec(t.Context(), `INSERT INTO counter (id, n) VALUES (1, 0)`)
	require.NoError(t, err)

	held := make(chan struct{})
	release := make(chan struct{})
	var holder error
	done := make(chan struct{})
	go func() {
		defer close(done)
		holder = run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
			if _, execErr := tx.Exec(ctx, `SELECT n FROM counter WHERE id = 1 FOR UPDATE`); execErr != nil {
				return execErr
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	err = run.InTx(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `UPDATE counter SET n = n + 1 WHERE id = 1`)
		return execErr
	})
	close(release)
	<-done

	require.NoError(t, holder)
	require.Error(t, err)
	assert.True(t, postgres.IsContention(err), "истёкший lock_timeout — 55P03: %v", err)
	assert.False(t, postgres.IsRetryable(err), "ожидание блокировки не повторяется само собой")
}

// Повторы кончаются: последняя ошибка приходит очищенной и с числом попыток.
func TestRunner_InTxRetry_GivesUp(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxAttempts = 3
	run, _ := newRunner(t, cfg)

	attempts := 0
	err := run.InTxRetry(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		attempts++
		// Настоящий 40001 от сервера: подделка не проверила бы классификацию.
		_, execErr := tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = 'serialization_failure'; END $$`)
		return execErr
	})

	require.Error(t, err)
	assert.Equal(t, 3, attempts)
	assert.Contains(t, err.Error(), "after 3 attempts")
	assert.Contains(t, err.Error(), "SQLSTATE 40001")
	assert.True(t, postgres.IsRetryable(err), "по ошибке видно, из-за чего сдались")
}

// Неповторяемая ошибка повторов не получает: вторая попытка стоила бы ещё
// одного круга по базе без единого шанса.
func TestRunner_InTxRetry_DoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()
	run, _ := newRunner(t, testConfig())

	attempts := 0
	sentinel := errors.New("бизнес-правило не выполнено")
	err := run.InTxRetry(t.Context(), func(_ context.Context, _ pgx.Tx) error {
		attempts++
		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, attempts)
}

func TestRunner_InTxRetry_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxAttempts = 100
	cfg.RetryBase = 200 * time.Millisecond
	run, _ := newRunner(t, cfg)

	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	start := time.Now()
	err := run.InTxRetry(ctx, func(ctx context.Context, tx pgx.Tx) error {
		attempts++
		if attempts == 1 {
			cancel()
		}
		_, execErr := tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = 'deadlock_detected'; END $$`)
		return execErr
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.LessOrEqual(t, attempts, 2)
	assert.Less(t, time.Since(start), 10*time.Second, "отмена не прервала сон между попытками")
}

// UTC-пин побеждает зону, заданную на сервере: дата-арифметика перестаёт
// зависеть от того, в каком образе поднят Postgres.
func TestWithUTC_BeatsServerTimezone(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	pool := pgtest.Schema(t, db)
	dbName := dbNameOf(t, pool)
	_, err := pool.Exec(t.Context(), `ALTER DATABASE `+dbName+` SET timezone = 'Asia/Novosibirsk'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(t.Context()), `ALTER DATABASE `+dbName+` RESET timezone`)
	})

	plain, err := pgx.Connect(t.Context(), db.DSN())
	require.NoError(t, err)
	defer func() { _ = plain.Close(context.WithoutCancel(t.Context())) }()
	var serverZone string
	require.NoError(t, plain.QueryRow(t.Context(), `SHOW timezone`).Scan(&serverZone))
	require.Equal(t, "Asia/Novosibirsk", serverZone, "настройка базы не применилась — тест ничего не доказывает")

	dsn, err := postgres.WithUTC(db.DSN())
	require.NoError(t, err)
	pinned, err := pgx.Connect(t.Context(), dsn)
	require.NoError(t, err)
	defer func() { _ = pinned.Close(context.WithoutCancel(t.Context())) }()

	var zone string
	require.NoError(t, pinned.QueryRow(t.Context(), `SHOW timezone`).Scan(&zone))
	assert.Equal(t, "UTC", zone)
}

// GUC приложения доезжает до сервера: по нему RLS-политика узнаёт роль.
func TestWithRuntimeParam_ReachesServer(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	dsn, err := postgres.WithRuntimeParam(db.DSN(), "application_name", "rebar-postgres-test")
	require.NoError(t, err)
	conn, err := pgx.Connect(t.Context(), dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	var name string
	require.NoError(t, conn.QueryRow(t.Context(), `SHOW application_name`).Scan(&name))
	assert.Equal(t, "rebar-postgres-test", name)
}

func dbNameOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&name))
	return name
}
