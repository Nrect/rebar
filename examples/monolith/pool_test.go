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
	require.Equal(t, "Asia/Kathmandu", timeZoneOf(t, raw), "контроль: тот же DSN мимо Open")

	sdb, err := shoppg.Open(t.Context(), dsn, postgres.Config{
		LockTimeout: 3 * time.Second, StatementTimeout: 10 * time.Second,
		MaxAttempts: 1, RetryBase: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(sdb.Close)
	require.Equal(t, "UTC", timeZoneOf(t, sdb.Pool), "пул shoppg.Open")
}

// timeZoneOf — TimeZone сессии на соединении пула.
func timeZoneOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var zone string
	require.NoError(t, pool.QueryRow(t.Context(), "SHOW TimeZone").Scan(&zone))
	return zone
}
