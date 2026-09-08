package outboxpg_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// secretPayload — «тело события» тестов: ищем его в текстах ошибок.
const secretPayload = "SECRET-TOKEN-42"

// testKind — тип по умолчанию; second — второй тип для фильтра Claim.
const (
	testKind   outbox.Kind = "order.paid"
	secondKind outbox.Kind = "receipt.send"
)

// db — база на весь тестовый бинарь; каждый тест заводит через неё свою схему,
// поэтому тесты идут параллельно и не видят строк друг друга.
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
	db.Close(context.Background())
	os.Exit(code)
}

// newStore — схема на тест плюс пул с search_path в неё; Up применяется из
// schema.sql, чтобы тестировался артефакт, а не его копия в коде.
func newStore(t *testing.T) (*outboxpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	return outboxpg.New(pool), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// envelope — конверт в том виде, в каком его отдаёт outbox.Producer.Prepare.
func envelope(mods ...func(*outbox.Envelope)) outbox.Envelope {
	return envelopeAt(pgtest.Now(), mods...)
}

// envelopeAt — тот же конверт, но все времена от одного момента: тесты аренды
// сравнивают available_at ровно с тем now, который уходит в Claim, и разница
// в микросекунды между двумя вызовами часов делала бы их плавающими.
func envelopeAt(now time.Time, mods ...func(*outbox.Envelope)) outbox.Envelope {
	id := uuid.New()
	env := outbox.Envelope{
		ID:            id,
		Kind:          testKind,
		Payload:       json.RawMessage(`{"token":"` + secretPayload + `","order":42}`),
		DedupKey:      "order:" + id.String(),
		AggregateType: "order",
		AggregateID:   id.String(),
		SchemaVersion: 1,
		Headers:       map[string]string{"traceparent": traceparent},
		Fingerprint:   fingerprintOf(id),
		Status:        outbox.StatusPending,
		AvailableAt:   now,
		OccurredAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, mod := range mods {
		mod(&env)
	}
	return env
}

// traceparent — сквозной контекст конверта: заголовки едут в трассировку.
const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// fingerprintOf — детерминированный отпечаток теста: ядро считает свой, а
// адаптеру важно лишь то, что байты вернутся неизменными.
func fingerprintOf(id uuid.UUID) []byte {
	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = id[i%len(id)]
	}
	return sum
}

// row — строка так, как её видит база: проверки Finish идут мимо адаптера.
type row struct {
	Status      string
	Payload     json.RawMessage
	Attempts    int
	AvailableAt time.Time
	ClaimToken  *uuid.UUID
	LockedUntil *time.Time
	LastError   string
	FailReason  string
	UpdatedAt   time.Time
	DoneAt      *time.Time
}

func readRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) row {
	t.Helper()
	var r row
	err := pool.QueryRow(t.Context(), `SELECT status, payload, attempts, available_at, claim_token,
		locked_until, last_error, fail_reason, updated_at, done_at
		FROM outbox_messages WHERE id = $1`, id).
		Scan(&r.Status, &r.Payload, &r.Attempts, &r.AvailableAt, &r.ClaimToken, &r.LockedUntil,
			&r.LastError, &r.FailReason, &r.UpdatedAt, &r.DoneAt)
	require.NoError(t, err)
	return r
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), query, args...).Scan(&n))
	return n
}

// mustEnqueue — вставка, которая обязана удаться: подготовка данных теста.
func mustEnqueue(t *testing.T, store *outboxpg.Store, env outbox.Envelope) outbox.Envelope {
	t.Helper()
	res, err := store.Enqueue(t.Context(), env)
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	return res.Envelope
}

// mustClaim — захват пачки под новым токеном; возвращает строки и токен.
func mustClaim(t *testing.T, store *outboxpg.Store, now time.Time, limit int, kinds ...outbox.Kind) ([]outbox.Envelope, uuid.UUID) {
	t.Helper()
	if len(kinds) == 0 {
		kinds = []outbox.Kind{testKind}
	}
	token := uuid.New()
	claimed, err := store.Claim(t.Context(), outbox.ClaimRequest{
		Now: now, Lease: time.Minute, Limit: limit, Kinds: kinds, Token: token,
	})
	require.NoError(t, err)
	return claimed, token
}

// assertSameEnvelope — конверт после круга через базу. Payload сравнивается
// КАК JSON, а не побайтно: колонка объявлена JSONB, и Postgres нормализует
// пробелы и порядок ключей. Тождество сообщения от этого не страдает —
// отпечаток лежит отдельной колонкой BYTEA и возвращается байт в байт;
// именно поэтому ядро хранит его, а не пересчитывает по прочитанному payload.
func assertSameEnvelope(t *testing.T, want, got outbox.Envelope) {
	t.Helper()
	assert.JSONEq(t, string(want.Payload), string(got.Payload))
	want.Payload, got.Payload = nil, nil
	assert.Equal(t, want, got)
}

// assertConstraintViolation — нарушение именно этого CHECK: имя ограничения —
// контракт схемы, а не деталь реализации.
func assertConstraintViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}

// finishedRow — строка, доведённая до нужного исхода: вставка, захват, Finish.
func finishedRow(t *testing.T, store *outboxpg.Store, now time.Time, req outbox.FinishRequest) outbox.Envelope {
	t.Helper()
	env := mustEnqueue(t, store, envelopeAt(now, func(e *outbox.Envelope) { e.DedupKey = "" }))
	claimed, token := mustClaim(t, store, now, 1)
	require.Len(t, claimed, 1)
	require.Equal(t, env.ID, claimed[0].ID, "захвачена не та строка: тест не изолирован")
	req.ID, req.Token, req.Now = env.ID, token, now
	require.NoError(t, store.Finish(t.Context(), req))
	return env
}
