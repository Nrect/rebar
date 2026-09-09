package paymentpg_test

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

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// postgresImage — digest-пин, как у mailpg.
const postgresImage = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

// envDatabaseURL — общий сервер вместо контейнера. Нужен прогону мутантов:
// иначе каждый мутант поднимает свой Postgres, и прогон врёт таймаутами.
const envDatabaseURL = "TEST_DATABASE_URL"

// maxPoolConns — потолок соединений на пул: тестам гонки нужно несколько
// параллельных транзакций, а сумма пулов обязана остаться ниже
// max_connections сервера.
const maxPoolConns = 6

// secretReference — «содержимое строки» тестов: ищем его в текстах ошибок.
const secretReference = "order:SECRET-42"

// testCurrency — валюта тестов; вторая нужна ровно для проверки, что чужая
// валюта не считается совпадением по числу.
const (
	testCurrency  = "RUB"
	otherCurrency = "KZT"
)

// adminPool — общий пул к базе прогона; каждый тест заводит через него свою
// схему, поэтому тесты идут параллельно и не видят строк друг друга.
var adminPool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	dsn, stop, err := startPostgres(ctx)
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
	stop(context.Background())
	os.Exit(code)
}

// startPostgres — общий сервер из TEST_DATABASE_URL либо свой контейнер.
func startPostgres(ctx context.Context) (dsn string, stop func(context.Context), err error) {
	if server := os.Getenv(envDatabaseURL); server != "" {
		return server, func(context.Context) {}, nil
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "payment",
				"POSTGRES_PASSWORD": "payment",
				"POSTGRES_DB":       "payment",
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
		return "", nil, err
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", nil, err
	}
	port, err := ctr.MappedPort(ctx, "5432")
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("postgres://payment:payment@%s:%d/payment", host, port.Num()),
		func(ctx context.Context) { _ = ctr.Terminate(ctx) }, nil
}

// newStore — схема на тест плюс адаптер над ней. Up применяется из schema.sql,
// чтобы тестировался артефакт, который уедет в миграции потребителя, а не его
// копия в коде.
func newStore(t *testing.T, opts paymentpg.Options) (*paymentpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	_, err := pool.Exec(t.Context(), schemaUp(t))
	require.NoError(t, err, "применение -- +goose Up из schema.sql")
	return paymentpg.New(pool, opts), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker или " + envDatabaseURL)
	}
	ctx := context.Background()
	schema := "t" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "создание схемы теста")

	pool, err := newPool(ctx, adminPool.Config().ConnString(), schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		// Схема убирается за собой: на общем сервере (TEST_DATABASE_URL) её
		// иначе накопится по одной на тест на каждый прогон.
		_, _ = adminPool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
	return pool
}

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

// intent — намерение в том виде, в каком его отдаёт payment.Service.Start.
func intent(mods ...func(*payment.Intent)) payment.Intent {
	id := uuid.New()
	now := testNow()
	in := payment.Intent{
		ID:                id,
		PayerID:           uuid.New(),
		Reference:         "order:" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		AmountMinor:       79900,
		Currency:          testCurrency,
		Items:             []payment.OrderItem{{Position: 0, ProductID: "sku-1", Title: "Курс", AmountMinor: 79900, Quantity: 1}},
		Provider:          "psfake",
		Method:            "bank_card",
		AutoCapture:       true,
		Status:            payment.StatusCreated,
		IdempotencyKey:    "key-" + uuid.NewString(),
		ParamsFingerprint: fingerprint(0xA5),
		CreatedAt:         now,
		UpdatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}
	for _, mod := range mods {
		mod(&in)
	}
	return in
}

// fingerprint — 32 байта, как sha256 параметров покупки: короче схема не примет.
func fingerprint(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// event — событие провайдера про намерение.
func event(in payment.Intent, kind payment.EventType, mods ...func(*payment.Event)) payment.Event {
	ev := payment.Event{
		Provider:          in.Provider,
		ProviderEventID:   string(in.Provider) + ":" + in.ID.String() + ":" + string(kind),
		ProviderPaymentID: "pay_" + in.ID.String(),
		IntentID:          in.ID,
		Type:              kind,
		AmountMinor:       in.AmountMinor,
		Currency:          in.Currency,
		OccurredAt:        testNow(),
	}
	for _, mod := range mods {
		mod(&ev)
	}
	return ev
}

// captureEntry — строка зачисления так, как её собирает домен.
func captureEntry(in payment.Intent, ev payment.Event, now time.Time) *payment.LedgerEntry {
	return &payment.LedgerEntry{
		ID:              uuid.New(),
		IntentID:        in.ID,
		Kind:            payment.LedgerCapture,
		AmountMinor:     in.AmountMinor,
		Currency:        in.Currency,
		ProviderEventID: ev.ProviderEventID,
		IdempotencyKey:  "capture:" + in.ID.String() + ":" + ev.ProviderEventID,
		CreatedAt:       now,
	}
}

// refundEntry — встречная запись на часть зачисления.
func refundEntry(in payment.Intent, capture payment.LedgerEntry, amount int64, key string,
	now time.Time,
) payment.LedgerEntry {
	actor := uuid.New()
	reverses := capture.ID
	return payment.LedgerEntry{
		ID:              uuid.New(),
		IntentID:        in.ID,
		Kind:            payment.LedgerRefund,
		AmountMinor:     amount,
		Currency:        in.Currency,
		ReversesEntryID: &reverses,
		IdempotencyKey:  key,
		ActorID:         &actor,
		CreatedAt:       now,
	}
}

// applyRequest — запрос так, как его собирает домен: ExpectFrom это ВСЕ законные
// источники целевого статуса, а не прочитанный только что статус.
func applyRequest(in payment.Intent, ev payment.Event, to payment.Status,
	ledger *payment.LedgerEntry, now time.Time,
) payment.ApplyEventRequest {
	req := payment.ApplyEventRequest{
		IntentID:   in.ID,
		Event:      ev,
		ExpectFrom: expectFrom(to),
		To:         to,
		Now:        now,
	}
	if ledger != nil {
		req.ExpectAmountMinor = in.AmountMinor
		req.ExpectCurrency = in.Currency
		req.Ledger = ledger
	}
	return req
}

// expectFrom — статусы, из которых законен переход в to. Домен считает это по
// своей таблице переходов (statusesInto); здесь она повторена явно, потому что
// функция ядра неэкспортирована.
func expectFrom(to payment.Status) []payment.Status {
	from := make([]payment.Status, 0, len(payment.AllStatuses))
	for _, st := range payment.AllStatuses {
		if st.CanTransitionTo(to) {
			from = append(from, st)
		}
	}
	return from
}

func mustCreate(t *testing.T, store *paymentpg.Store, in payment.Intent) payment.Intent {
	t.Helper()
	require.NoError(t, store.CreateIntent(t.Context(), in))
	return in
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), query, args...).Scan(&n))
	return n
}
