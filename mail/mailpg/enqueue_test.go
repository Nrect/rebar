package mailpg_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/postgres"
)

func TestStore_Enqueue_InsertsEnvelopeAsIs(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	notAfter := testNow().Add(time.Hour)
	env := envelope(func(e *mail.Envelope) { e.NotAfter = &notAfter })

	res, err := store.Enqueue(context.Background(), env)

	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeInserted, res.Outcome)
	assert.Equal(t, env, res.Envelope, "конверт возвращается таким же, каким лёг")
	row := readRow(t, pool, env.ID)
	assert.Equal(t, string(mail.StatusPending), row.Status)
	assert.Equal(t, env.Headers, row.Headers)
	assert.Zero(t, row.Attempts)
	assert.Nil(t, row.LockedUntil)
	assert.Nil(t, row.SentAt)
}

// Повтор по ключу — успех с существующей строкой: законность решает домен по
// отпечатку, поэтому он обязан вернуться байт в байт.
func TestStore_Enqueue_DuplicateKeyReturnsExistingRow(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	first := mustEnqueue(t, store, envelope())
	second := envelope(func(e *mail.Envelope) {
		e.DedupKey = first.DedupKey
		e.Subject = "совсем другая тема"
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	})

	res, err := store.Enqueue(context.Background(), second)

	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeDuplicate, res.Outcome)
	assert.Equal(t, first.ID, res.Envelope.ID)
	assert.True(t, bytes.Equal(first.Fingerprint, res.Envelope.Fingerprint), "отпечаток байт в байт")
	assert.Equal(t, first, res.Envelope)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM email_outbox`))
}

// Тот же id с другим ключом — нарушение первичного ключа, а не дубль: арбитр
// ON CONFLICT указан колонкой, остальные UNIQUE остаются ошибкой.
func TestStore_Enqueue_SameIDDifferentKeyIsError(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	first := mustEnqueue(t, store, envelope())

	res, err := store.Enqueue(context.Background(), envelope(func(e *mail.Envelope) { e.ID = first.ID }))

	require.Error(t, err)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.NotEqual(t, mail.OutcomeDuplicate, res.Outcome)
	assert.Contains(t, err.Error(), "23505")
}

// Отвергнутая строка целиком уезжает в PgError.Detail вместе с телом письма:
// самый честный тест на утечку — INSERT, потому что тело в ней ещё есть.
// bodyConstraint — ограничение, которым база отбивает эту вставку. Строка с
// телом и чужим статусом нарушает СРАЗУ ДВА CHECK (словарь статусов и «тело
// стёрто в терминальном статусе»), а Postgres называет одно; проверено — он
// называет ограничение тела. Имя объявлено в schema.sql, поэтому это контракт,
// а не соглашение об именовании Postgres.
const bodyConstraint = "email_outbox_body_cleared_chk"

func TestStore_Enqueue_ErrorHidesBody(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	_, err := store.Enqueue(context.Background(),
		envelope(func(e *mail.Envelope) { e.Status = mail.Status("bogus") }))

	require.Error(t, err)
	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Contains(t, err.Error(), "23514")

	// ПРОВЕРКА ПО ТИПУ, А НЕ ПО ТЕКСТУ. Detail не печатается в Error(), он
	// достаётся только через errors.As, — поэтому проверка текста зелена и на
	// адаптере, который заворачивает *PgError целиком, то есть сторожит то,
	// чего в тексте не бывает. Ниже сперва доказывается, что утекать есть
	// чему, и только потом — что оно недосягаемо.
	var raw *pgconn.PgError
	require.ErrorAs(t, rawInsertError(t, pool), &raw)
	require.Contains(t, raw.Detail, secretLink, "в Detail драйвера лежит вся строка вместе с телом")
	require.Equal(t, bodyConstraint, raw.ConstraintName, "имя ограничения — контракт схемы")

	var leaked *pgconn.PgError
	assert.NotErrorAs(t, err, &leaked,
		"ошибка драйвера обязана быть снята с цепочки: в её Detail лежит тело письма")
	assert.NotContains(t, err.Error(), secretLink)
	assert.NotContains(t, err.Error(), "Failing row")

	// Вместо снятого *PgError наружу едет очищенная ошибка общей границы: по её
	// имени ограничения потребитель отличает один конфликт от другого — так же,
	// как у остальных адаптеров хранилища.
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "наружу едет очищенная ошибка postgres")
	assert.Equal(t, bodyConstraint, sanitized.Constraint)
	assert.NotContains(t, sanitized.Message, secretLink)
}

// rawInsertError — то же нарушение мимо адаптера: без него проверка
// недосягаемости ничего не значила бы.
func rawInsertError(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	env := envelope(func(e *mail.Envelope) { e.Status = mail.Status("bogus") })
	_, err := pool.Exec(context.Background(),
		`INSERT INTO email_outbox (id, kind, to_email, from_email, subject, body_text, body_html,
			dedup_key, fingerprint, message_id, status, next_attempt_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, $12)`,
		env.ID, string(env.Kind), env.To.Email, env.From.Email, env.Subject, env.Text, env.HTML,
		env.DedupKey, env.Fingerprint, env.MessageID, string(env.Status), testNow())
	require.Error(t, err, "нарушение CHECK статуса ожидается")
	return err
}

// Контракт «в одной транзакции» (CONVENTIONS §5): строка outbox и факт
// потребителя живут и умирают вместе.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE consumer_fact (id UUID PRIMARY KEY)`)
	require.NoError(t, err)

	rolled := envelope()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, rolled.ID)
	require.NoError(t, err)
	res, err := store.WithTx(tx).Enqueue(ctx, rolled)
	require.NoError(t, err)
	require.Equal(t, mail.OutcomeInserted, res.Outcome)
	require.NoError(t, tx.Rollback(ctx))

	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, rolled.ID))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM consumer_fact WHERE id = $1`, rolled.ID))

	committed := envelope()
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, committed.ID)
	require.NoError(t, err)
	_, err = store.WithTx(tx).Enqueue(ctx, committed)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM email_outbox WHERE id = $1`, committed.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM consumer_fact WHERE id = $1`, committed.ID))
}

// Повтор по ключу не должен ронять транзакцию потребителя: перехват 23505
// перевёл бы её в aborted и снёс бы бизнес-факт вместе с письмом.
func TestStore_WithTx_DuplicateKeepsTransactionUsable(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE consumer_fact (id UUID PRIMARY KEY)`)
	require.NoError(t, err)
	first := mustEnqueue(t, store, envelope())

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	res, err := store.WithTx(tx).Enqueue(ctx, envelope(func(e *mail.Envelope) { e.DedupKey = first.DedupKey }))
	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeDuplicate, res.Outcome)
	_, err = tx.Exec(ctx, `INSERT INTO consumer_fact (id) VALUES ($1)`, res.Envelope.ID)
	require.NoError(t, err, "транзакция жива после дубля")
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM consumer_fact`))
}
