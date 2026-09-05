package mailpg_test

import (
	"bytes"
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

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
)

// postgresImage — digest-пин, как у Mailpit в smtp.
const postgresImage = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

// secretLink — «тело письма» тестов: ищем его в текстах ошибок.
const secretLink = "https://example.ru/verify?token=SECRET-TOKEN-42"

// maxPoolConns — потолок соединений на пул; четыре нужны тесту гонки Claim.
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
				"POSTGRES_USER":     "mail",
				"POSTGRES_PASSWORD": "mail",
				"POSTGRES_DB":       "mail",
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
	return ctr, fmt.Sprintf("postgres://mail:mail@%s:%d/mail", host, port.Num()), nil
}

// newStore — схема на тест плюс пул с search_path в неё; Up применяется из
// schema.sql, чтобы тестировался артефакт, а не его копия в коде.
func newStore(t *testing.T) (*mailpg.Store, *pgxpool.Pool) {
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

	_, err = pool.Exec(ctx, schemaUp(t))
	require.NoError(t, err, "применение -- +goose Up из schema.sql")
	return mailpg.New(pool), pool
}

// newPool — пул к базе контейнера, при непустой schema — с search_path в неё.
// maxPoolConns держит сумму пулов параллельных тестов ниже max_connections
// Postgres (по умолчанию 100).
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

// envelope — конверт в том виде, в каком его отдаёт mail.Service.Prepare.
func envelope(mods ...func(*mail.Envelope)) mail.Envelope {
	id := uuid.New()
	now := testNow()
	env := mail.Envelope{
		ID:            id,
		Kind:          "verify",
		To:            mail.Address{Email: "teacher@school.ru", Name: "Учитель"},
		From:          mail.Address{Email: "noreply@example.ru", Name: "Планета чтения"},
		Subject:       "Подтверждение почты",
		Text:          "Ссылка: " + secretLink,
		HTML:          `<p><a href="` + secretLink + `">подтвердить</a></p>`,
		Headers:       map[string]string{"Reply-To": "support@example.ru"},
		DedupKey:      "verify:" + id.String(),
		Fingerprint:   bytes.Repeat([]byte{0xA5}, 32),
		MessageID:     "<" + id.String() + "@example.ru>",
		Status:        mail.StatusPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, mod := range mods {
		mod(&env)
	}
	return env
}

// outboxRow — строка так, как её видит база: проверки Finish идут мимо адаптера.
type outboxRow struct {
	Status            string
	Subject           string
	Text              string
	HTML              string
	Headers           map[string]string
	Attempts          int
	NextAttemptAt     time.Time
	LockedUntil       *time.Time
	LastError         string
	FailReason        string
	Transport         string
	ProviderMessageID string
	UpdatedAt         time.Time
	SentAt            *time.Time
}

func readRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) outboxRow {
	t.Helper()
	var r outboxRow
	err := pool.QueryRow(context.Background(), `SELECT status, subject, body_text, body_html,
		headers, attempts, next_attempt_at, locked_until, last_error, fail_reason, transport,
		provider_message_id, updated_at, sent_at FROM email_outbox WHERE id = $1`, id).
		Scan(&r.Status, &r.Subject, &r.Text, &r.HTML, &r.Headers, &r.Attempts, &r.NextAttemptAt,
			&r.LockedUntil, &r.LastError, &r.FailReason, &r.Transport, &r.ProviderMessageID,
			&r.UpdatedAt, &r.SentAt)
	require.NoError(t, err)
	return r
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// mustEnqueue — вставка, которая обязана удаться: подготовка данных теста.
func mustEnqueue(t *testing.T, store *mailpg.Store, env mail.Envelope) mail.Envelope {
	t.Helper()
	res, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, mail.OutcomeInserted, res.Outcome)
	return res.Envelope
}
