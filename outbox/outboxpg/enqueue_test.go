package outboxpg_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

func TestStore_Enqueue_InsertsEnvelopeAsIs(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	notAfter := pgtest.Now().Add(time.Hour)
	env := envelope(func(e *outbox.Envelope) { e.NotAfter = &notAfter })

	res, err := store.Enqueue(t.Context(), env)

	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, res.Outcome)
	assertSameEnvelope(t, env, res.Envelope)
	got := readRow(t, pool, env.ID)
	assert.Equal(t, string(outbox.StatusPending), got.Status)
	assert.JSONEq(t, string(env.Payload), string(got.Payload))
	assert.Zero(t, got.Attempts)
	assert.Nil(t, got.ClaimToken)
	assert.Nil(t, got.LockedUntil)
	assert.Nil(t, got.DoneAt)
}

// Повтор по ключу — успех с существующей строкой: законность решает домен по
// отпечатку, поэтому он обязан вернуться байт в байт.
func TestStore_Enqueue_DuplicateKeyReturnsExistingRow(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	first := mustEnqueue(t, store, envelope())
	second := envelope(func(e *outbox.Envelope) {
		e.DedupKey = first.DedupKey
		e.Payload = json.RawMessage(`{"order":777}`)
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	})

	res, err := store.Enqueue(t.Context(), second)

	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeDuplicate, res.Outcome)
	assert.Equal(t, first.ID, res.Envelope.ID)
	assert.True(t, bytes.Equal(first.Fingerprint, res.Envelope.Fingerprint), "отпечаток байт в байт")
	assertSameEnvelope(t, first, res.Envelope)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

// Уникальность — парой: один и тот же "order:42" законен для разных Kind.
func TestStore_Enqueue_SameKeyDifferentKindIsNotDuplicate(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	first := mustEnqueue(t, store, envelope())

	res, err := store.Enqueue(t.Context(), envelope(func(e *outbox.Envelope) {
		e.Kind, e.DedupKey = secondKind, first.DedupKey
	}))

	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeInserted, res.Outcome)
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

// Пустой ключ дедупу не подлежит: частичный индекс его не покрывает, и
// событие без естественного ключа не должно его выдумывать.
func TestStore_Enqueue_EmptyDedupKeyAlwaysInserts(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	for range 3 {
		res, err := store.Enqueue(t.Context(), envelope(func(e *outbox.Envelope) { e.DedupKey = "" }))
		require.NoError(t, err)
		require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	}

	assert.Equal(t, 3, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

// Тот же id — нарушение первичного ключа, а не дубль: арбитр ON CONFLICT
// указан колонками индекса дедупа, остальные UNIQUE остаются ошибкой.
func TestStore_Enqueue_SameIDIsError(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	first := mustEnqueue(t, store, envelope())

	res, err := store.Enqueue(t.Context(), envelope(func(e *outbox.Envelope) { e.ID = first.ID }))

	require.Error(t, err)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	assert.NotEqual(t, outbox.OutcomeDuplicate, res.Outcome)
	assert.Contains(t, err.Error(), "23505")
}

// Отвергнутая строка целиком уезжает в PgError.Detail вместе с payload:
// самый честный тест на утечку — INSERT, потому что payload в ней ещё есть.
//
// ГЛАВНАЯ ПРОВЕРКА ЗДЕСЬ — НЕ ТЕКСТ, А ДОСЯГАЕМОСТЬ *pgconn.PgError ЧЕРЕЗ
// errors.As. Detail не входит в PgError.Error(), поэтому тест, который ищет
// секрет только в тексте, зелен и на адаптере БЕЗ границы: утечка происходит
// ниже по стеку, там, где кто-то достаёт *PgError и логирует его целиком.
// Ровно это и закрывает postgres.Sanitize, не заворачивая *PgError в цепочку.
func TestStore_Enqueue_ErrorHidesPayload(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	bad := envelope(func(e *outbox.Envelope) { e.SchemaVersion = 0 })

	// Сначала — что утекать есть чему: сырая ошибка того же INSERT несёт
	// payload в Detail. Без этой половины тест не доказывал бы ничего.
	raw := rawInsertError(t, pool, bad)
	var rawPg *pgconn.PgError
	require.ErrorAs(t, raw, &rawPg)
	require.Contains(t, rawPg.Detail, secretPayload, "иначе проверять нечего: Detail пуст")

	_, err := store.Enqueue(t.Context(), bad)

	require.Error(t, err)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	var leaked *pgconn.PgError
	assert.NotErrorAs(t, err, &leaked,
		"*pgconn.PgError достаётся через errors.As — вместе с ним достаётся и Detail со всей строкой")
	assert.NotContains(t, err.Error(), secretPayload)
	assert.NotContains(t, err.Error(), "Failing row")

	// Классификация после границы жива: SQLSTATE и имя ограничения — это
	// имена схемы, а не данные, и они обязаны пережить Sanitize.
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "23514", sanitized.Code)
	assert.NotEmpty(t, sanitized.Message, "сообщение Postgres остаётся: по нему чинят")
	assert.Contains(t, err.Error(), "23514")
}

// rawInsertError — та же вставка мимо адаптера: сырая ошибка Postgres со всем,
// что адаптер обязан отрезать.
func rawInsertError(t *testing.T, pool *pgxpool.Pool, env outbox.Envelope) error {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		`INSERT INTO outbox_messages (id, kind, payload, schema_version, fingerprint, status,
			available_at, occurred_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'pending', $6, $6, $6, $6)`,
		env.ID, env.Kind, env.Payload, env.SchemaVersion, env.Fingerprint, env.CreatedAt)
	require.Error(t, err, "вставка обязана быть отвергнута CHECK")
	return err
}

// Контракт «в одной транзакции» (CONVENTIONS §5): строка очереди и факт
// потребителя живут и умирают вместе — включая занятый ключ дедупа, иначе
// следующий законный повтор операции будет молча отвергнут.
func TestEnqueue_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()
	_, pool := newStore(t)
	ctx := t.Context()
	_, err := pool.Exec(ctx, `CREATE TABLE consumer_fact (id UUID PRIMARY KEY)`)
	require.NoError(t, err)

	rolled := envelope()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, rolled.ID)
	require.NoError(t, err)
	res, err := outboxpg.Enqueue(ctx, tx, rolled)
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	require.NoError(t, tx.Rollback(ctx))

	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, rolled.ID))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM consumer_fact WHERE id = $1`, rolled.ID))
	assert.Zero(t, countRows(t, pool,
		`SELECT count(*) FROM outbox_messages WHERE dedup_key = $1`, rolled.DedupKey),
		"откат унёс и строку дедупа: иначе ключ остался бы занят навсегда")

	committed := envelope()
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, committed.ID)
	require.NoError(t, err)
	_, err = outboxpg.Enqueue(ctx, tx, committed)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages WHERE id = $1`, committed.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM consumer_fact WHERE id = $1`, committed.ID))
}

// Законный повтор события не должен ронять транзакцию потребителя: перехват
// 23505 перевёл бы её в aborted и снёс бы бизнес-факт вместе с событием.
func TestEnqueue_DuplicateDoesNotAbortTx(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := t.Context()
	_, err := pool.Exec(ctx, `CREATE TABLE consumer_fact (id UUID PRIMARY KEY)`)
	require.NoError(t, err)
	first := mustEnqueue(t, store, envelope())
	// Тот же ключ и то же содержимое: повтор законен, потому что отпечаток тот же.
	repeat := envelope(func(e *outbox.Envelope) {
		e.DedupKey, e.Fingerprint = first.DedupKey, first.Fingerprint
	})

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	res, err := outboxpg.Enqueue(ctx, tx, repeat)
	require.NoError(t, err, "повтор — успех, а не ошибка")
	assert.Equal(t, outbox.OutcomeDuplicate, res.Outcome)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, repeat.ID)
	require.NoError(t, err, "транзакция жива после дубля")
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM consumer_fact`), "бизнес-факт уцелел")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

// Тот же ключ на другое сообщение — громкий отказ, а не тихий no-op: сверку
// делает пакетная функция, а не память вызывающего.
func TestEnqueue_DifferentMessageUnderSameKeyIsLoud(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := t.Context()
	first := mustEnqueue(t, store, envelope())

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = outboxpg.Enqueue(ctx, tx, envelope(func(e *outbox.Envelope) {
		e.DedupKey = first.DedupKey
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	}))

	require.ErrorIs(t, err, outbox.ErrKeyReused)
	assert.NotContains(t, err.Error(), secretPayload, "payload не попадает в текст ошибки")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM outbox_messages`))
}

func TestEnqueue_PanicsOnNilTx(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { _, _ = outboxpg.Enqueue(t.Context(), nil, envelope()) })
}

// JSONB нормализует пробелы и порядок ключей — это свойство колонки, а не
// потеря тождества: отпечаток лежит отдельной колонкой BYTEA, возвращается
// байт в байт, и по нему решается законность повтора.
func TestStore_Enqueue_JSONBNormalizesPayloadButFingerprintSurvives(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	env := envelope(func(e *outbox.Envelope) {
		e.Payload = json.RawMessage(`{"b":2,   "a":1}`)
	})

	res, err := store.Enqueue(t.Context(), env)

	require.NoError(t, err)
	assert.NotEqual(t, string(env.Payload), string(res.Envelope.Payload), "байты нормализованы базой")
	assert.JSONEq(t, string(env.Payload), string(res.Envelope.Payload), "значение то же")
	assert.True(t, bytes.Equal(env.Fingerprint, res.Envelope.Fingerprint), "отпечаток байт в байт")
}
