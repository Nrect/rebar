package monolith_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// testService — application_name пула в тестах пакета.
const testService = "monolith-test"

// TestOpen_PinsUTC — пул shoppg.Open в UTC, даже когда DSN просит чужой пояс.
// Контроль — тот же DSN мимо Open: без него тест не отличил бы пин от сервера,
// который и так в UTC. Пояс — параметром DSN, а не ALTER DATABASE: соседние
// тесты идут на той же базе.
func TestOpen_PinsUTC(t *testing.T) {
	pgtest.Short(t)
	dsn, err := postgres.WithRuntimeParam(pgtest.SchemaDSN(t, db), "timezone", "Asia/Kathmandu")
	require.NoError(t, err)

	raw, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	require.Equal(t, "Asia/Kathmandu", show(t, raw, "TimeZone"), "контроль: тот же DSN мимо Open")

	require.Equal(t, "UTC", show(t, openShop(t, dsn).Pool, "TimeZone"), "пул shoppg.Open")
}

// TestOpen_PinsSessionParams — application_name и срок забытой транзакции Open
// ставит поверх пришедших в DSN. Контроль — тот же DSN мимо Open.
func TestOpen_PinsSessionParams(t *testing.T) {
	pgtest.Short(t)
	dsn := pgtest.SchemaDSN(t, db)
	for _, p := range [][2]string{
		{"application_name", "foreign-app"},
		{"idle_in_transaction_session_timeout", "5min"},
	} {
		var err error
		dsn, err = postgres.WithRuntimeParam(dsn, p[0], p[1])
		require.NoError(t, err)
	}

	raw, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	require.Equal(t, "foreign-app", show(t, raw, "application_name"), "контроль: тот же DSN мимо Open")
	require.Equal(t, "5min", show(t, raw, "idle_in_transaction_session_timeout"), "контроль: тот же DSN мимо Open")

	pool := openShop(t, dsn).Pool
	require.Equal(t, testService, show(t, pool, "application_name"), "application_name пула shoppg.Open")
	require.Equal(t, "1min", show(t, pool, "idle_in_transaction_session_timeout"),
		"idle_in_transaction_session_timeout пула shoppg.Open")
}

// openShop — пул shoppg.Open на dsn, открытый так же, как сборка.
func openShop(t *testing.T, dsn string) *shoppg.DB {
	t.Helper()
	sdb, err := shoppg.Open(t.Context(), dsn, testService, postgres.Config{
		LockTimeout: 3 * time.Second, StatementTimeout: 10 * time.Second,
		MaxAttempts: 1, RetryBase: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(sdb.Close)
	return sdb
}

// show — значение параметра сессии на соединении пула.
func show(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var value string
	require.NoError(t, pool.QueryRow(t.Context(), "SHOW "+name).Scan(&value))
	return value
}
