package authpg_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/postgres/pgtest"
)

// ЭФФЕКТ ПОТРЕБИТЕЛЯ И СТРОКА ТОКЕНА ЖИВУТ И УМИРАЮТ ВМЕСТЕ. Вставка мимо
// транзакции компилируется, проходит тесты и оставляет ссылки в никуда, а
// гашение мимо транзакции — подтверждённый токен при неподтверждённом адресе.
// Тест читает из базы ПОСЛЕ отката, как того требует CORRECTNESS §7.
// БЕЗ t.Parallel У РОДИТЕЛЯ: подтесты идут по порядку на одной строке
// пользователя — второй проверяет, что эффект третьего ещё не применён.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	tokens := store.Tokens()
	now := pgtest.Now()

	t.Run("выдача: строка токена и эффект потребителя откатываются вместе", func(t *testing.T) {
		row := tokenRow(subject, token.PurposeVerify, now)
		rollback(t, pool, func(tx pgx.Tx) error {
			require.NoError(t, tokens.WithTx(tx).Insert(t.Context(), row))
			_, err := tx.Exec(t.Context(),
				`UPDATE consumer_users SET login = $2 WHERE id = $1`, subject, "moved@example.invalid")
			return err
		})

		assert.Zero(t, countRows(t, pool,
			`SELECT count(*) FROM auth_tokens WHERE realm = $1 AND token_hash = $2`,
			string(testRealm), row.TokenHash), "строка токена пережила откат")
		assert.Equal(t, 1, countRows(t, pool,
			`SELECT count(*) FROM consumer_users WHERE id = $1 AND login = $2`,
			subject, "alice@example.invalid"), "эффект потребителя пережил откат")
	})

	t.Run("гашение: токен и эффект откатываются вместе", func(t *testing.T) {
		row := tokenRow(subject, token.PurposeVerify, now)
		require.NoError(t, tokens.Insert(t.Context(), row))

		rollback(t, pool, func(tx pgx.Tx) error {
			res, err := tokens.WithTx(tx).ConsumeRow(t.Context(), consumeReq(row, now))
			require.NoError(t, err)
			require.Equal(t, subject, res.SubjectID)
			_, err = tx.Exec(t.Context(), `UPDATE consumer_users SET verified = true WHERE id = $1`, subject)
			return err
		})

		assert.False(t, verifiedFlag(t, pool, subject), "эффект пережил откат")
		// Токен обязан остаться ЖИВЫМ: погашенный при неприменённом эффекте —
		// это человек без подтверждения и без ссылки.
		res, err := tokens.ConsumeRow(t.Context(), consumeReq(row, now))
		require.NoError(t, err, "токен обязан пережить откат живым")
		assert.Equal(t, subject, res.SubjectID)
	})

	t.Run("коммит доводит оба эффекта", func(t *testing.T) {
		row := tokenRow(subject, token.PurposeVerify, now)
		require.NoError(t, tokens.Insert(t.Context(), row))

		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		_, err = tokens.WithTx(tx).ConsumeRow(t.Context(), consumeReq(row, now))
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), `UPDATE consumer_users SET verified = true WHERE id = $1`, subject)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(t.Context()))

		assert.True(t, verifiedFlag(t, pool, subject))
		_, err = tokens.ConsumeRow(t.Context(), consumeReq(row, now))
		assert.ErrorIs(t, err, session.ErrTokenInvalid, "погашенный токен не открывается повторно")
	})
}

// ОДНОРАЗОВОСТЬ ПОД ГОНКОЙ. Арбитр — база: восемь транзакций на один токен,
// выигрывает ровно одна. Пара «прочитал, потом записал» на этом месте отдала
// бы токен обоим — и подтверждение адреса, и сброс пароля сработали бы дважды.
func TestConsume_IsOnceUnderRace(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	tokens := store.Tokens()
	now := pgtest.Now()
	row := tokenRow(subject, token.PurposeVerify, now)
	require.NoError(t, tokens.Insert(t.Context(), row))

	const racers = 8
	results := make(chan error, racers)
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			start.Wait()
			results <- consumeInTx(context.WithoutCancel(t.Context()), pool, tokens, consumeReq(row, now), subject)
		}()
	}
	start.Done()
	wg.Wait()
	close(results)

	var won int
	for err := range results {
		if err == nil {
			won++
			continue
		}
		require.ErrorIs(t, err, session.ErrTokenInvalid, "проигравший обязан получить именно «токен негоден»")
	}
	assert.Equal(t, 1, won, "токен обязан достаться ровно одному")
	assert.True(t, verifiedFlag(t, pool, subject), "победитель обязан был применить эффект")
	assert.Equal(t, 1, countRows(t, pool,
		`SELECT count(*) FROM auth_tokens WHERE realm = $1 AND token_hash = $2 AND used_at IS NOT NULL`,
		string(testRealm), row.TokenHash))
}

// Истёкший токен не гасится: срок проверяет тот же предикат, что и
// одноразовость, поэтому обойти его отдельным запросом нельзя.
// Без t.Parallel у родителя: утверждение после цикла считает строки, которые
// оставили подтесты.
func TestConsumeRow_RefusesExpiredAndForeign(t *testing.T) {
	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	tokens := store.Tokens()
	now := pgtest.Now()
	row := tokenRow(subject, token.PurposeVerify, now)
	require.NoError(t, tokens.Insert(t.Context(), row))

	for name, req := range map[string]session.ConsumeRequest{
		"истёк":            consumeReq(row, row.ExpiresAt.Add(time.Second)),
		"ровно в срок":     consumeReq(row, row.ExpiresAt),
		"чужой реалм":      withRealm(consumeReq(row, now), otherRealm),
		"чужое назначение": withPurpose(consumeReq(row, now), token.PurposeReset),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.ConsumeRow(t.Context(), req)
			assert.ErrorIs(t, err, session.ErrTokenInvalid)
		})
	}
	assert.Equal(t, 1, countRows(t, pool,
		`SELECT count(*) FROM auth_tokens WHERE realm = $1 AND used_at IS NULL`, string(testRealm)),
		"ни одна неудачная попытка не должна была погасить токен")
}

// Отзыв гасит все живые токены назначения и НЕ трогает ни чужой реалм, ни
// чужое назначение: смена пароля не должна отменять подтверждение адреса.
func TestRevokeOfSubject_KillsOnlyItsOwn(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	tokens := store.Tokens()
	now := pgtest.Now()

	reset := tokenRow(subject, token.PurposeReset, now)
	verify := tokenRow(subject, token.PurposeVerify, now)
	foreign := tokenRow(subject, token.PurposeReset, now)
	foreign.Realm = otherRealm
	stranger := tokenRow(uuid.New(), token.PurposeReset, now)
	for _, row := range []session.OneTimeToken{reset, verify, foreign, stranger} {
		require.NoError(t, tokens.Insert(t.Context(), row))
	}

	n, err := tokens.RevokeOfSubject(t.Context(), testRealm, subject, token.PurposeReset, now)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, countRows(t, pool,
		`SELECT count(*) FROM auth_tokens WHERE used_at IS NULL`), "погашено больше, чем просили")

	// Повторный отзыв ничего не меняет: гасить нечего.
	again, err := tokens.RevokeOfSubject(t.Context(), testRealm, subject, token.PurposeReset, now)
	require.NoError(t, err)
	assert.Zero(t, again)
}

func TestPurgeExpired_RemovesOnlyStaleRowsOfRealm(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	tokens := store.Tokens()
	now := pgtest.Now()

	stale := tokenRow(subject, token.PurposeReset, now.Add(-2*time.Hour))
	fresh := tokenRow(subject, token.PurposeVerify, now)
	foreign := tokenRow(subject, token.PurposeReset, now.Add(-2*time.Hour))
	foreign.Realm = otherRealm
	for _, row := range []session.OneTimeToken{stale, fresh, foreign} {
		require.NoError(t, tokens.Insert(t.Context(), row))
	}

	n, err := tokens.PurgeExpired(t.Context(), testRealm, now)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM auth_tokens`))
}

// Назначение вне закрытого набора отбивает БАЗА: арбитр закрытого набора —
// CHECK, а не проверка в Go, которую можно обойти чужим кодом (CORRECTNESS §4).
func TestInsert_UnknownPurposeIsRefusedByTheSchema(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	subject := withConsumerUsers(t, pool)
	now := pgtest.Now()
	row := tokenRow(subject, "totp_enrol", now)

	err := store.Tokens().Insert(t.Context(), row)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM auth_tokens`))
}

func TestNewTokens_PanicsOnNil(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { authpg.NewTokens(nil) })
	assert.Panics(t, func() { authpg.New(nil) })

	store, _ := newStore(t)
	assert.Panics(t, func() { store.WithTx(nil) })
	assert.Panics(t, func() { store.Tokens().WithTx(nil) })
}

// rollback гоняет fn в транзакции и всегда откатывает её.
func rollback(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	err = fn(tx)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(t.Context()))
}

// consumeInTx — гашение плюс эффект потребителя одной транзакцией: ровно так
// это делает его адаптер порта Tokens.
func consumeInTx(ctx context.Context, pool *pgxpool.Pool, tokens *authpg.Tokens,
	req session.ConsumeRequest, subject uuid.UUID,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tokens.WithTx(tx).ConsumeRow(ctx, req); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE consumer_users SET verified = true WHERE id = $1`, subject); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tokenRow(subject uuid.UUID, purpose token.Purpose, now time.Time) session.OneTimeToken {
	return session.OneTimeToken{
		TokenHash: uuid.New().String(),
		Realm:     testRealm,
		Purpose:   purpose,
		SubjectID: subject,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
}

func consumeReq(row session.OneTimeToken, now time.Time) session.ConsumeRequest {
	return session.ConsumeRequest{
		Realm: row.Realm, Purpose: row.Purpose, TokenHash: row.TokenHash, Now: now,
	}
}

func withRealm(req session.ConsumeRequest, realm auth.Realm) session.ConsumeRequest {
	req.Realm = realm
	return req
}

func withPurpose(req session.ConsumeRequest, purpose token.Purpose) session.ConsumeRequest {
	req.Purpose = purpose
	return req
}

// errNoRows — pgx.ErrNoRows не должен утекать наружу: порт говорит о своих
// ошибках, а не о драйвере.
var errNoRows = pgx.ErrNoRows

func TestConsumeRow_DoesNotLeakDriverErrors(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	_, err := store.Tokens().ConsumeRow(t.Context(), session.ConsumeRequest{
		Realm: testRealm, Purpose: token.PurposeVerify, TokenHash: "нет такого", Now: pgtest.Now(),
	})

	require.ErrorIs(t, err, session.ErrTokenInvalid)
	assert.NotErrorIs(t, err, errNoRows, "ошибка драйвера наружу не выходит")
}
