package mailpg_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// secretLink — «тело письма» тестов: ищем его в текстах ошибок.
const secretLink = "https://example.ru/verify?token=SECRET-TOKEN-42"

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

// newStore — схема на тест плюс накат из schema.sql: тестируется артефакт,
// который уедет в миграции потребителя, а не его копия в коде.
func newStore(t *testing.T) (*mailpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	return mailpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// testNow — момент так, как его хранит timestamptz: UTC и микросекунды.
func testNow() time.Time { return pgtest.Now() }

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
