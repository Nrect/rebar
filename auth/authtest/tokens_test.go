package authtest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

const (
	tokenRealm auth.Realm = "suite"
	someLogin  string     = "alice@example.invalid"
	newLogin   string     = "alice.new@example.invalid"
)

func tokenNow() time.Time { return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC) }

// ДВОЙНИК ЧЕСТНО ПРИМЕНЯЕТ ЭФФЕКТ НАЗНАЧЕНИЯ. Двойник, который просто помечает
// токен использованным, оставляет зелёным тест «подтверждение адреса
// работает» при неработающем подтверждении.
func TestMemTokens_AppliesEffectOfEveryPurpose(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		purpose token.Purpose
		payload string
		newHash string
		check   func(t *testing.T, got auth.Identity)
	}{
		"подтверждение адреса": {
			purpose: token.PurposeVerify,
			check:   func(t *testing.T, got auth.Identity) { t.Helper(); assert.True(t, got.Verified) },
		},
		"сброс пароля": {
			purpose: token.PurposeReset,
			newHash: "$argon2id$v=19$new",
			check: func(t *testing.T, got auth.Identity) {
				t.Helper()
				assert.Equal(t, "$argon2id$v=19$new", got.PasswordHash)
			},
		},
		"смена логина": {
			purpose: token.PurposeEmailChange,
			payload: newLogin,
			check:   func(t *testing.T, got auth.Identity) { t.Helper(); assert.Equal(t, newLogin, got.Login) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ids, tokens, id := newTokenStand(t)
			row := tokenRow(id, tc.purpose, tc.payload)
			require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{}))

			res, err := tokens.Consume(t.Context(), session.ConsumeRequest{
				Realm: tokenRealm, Purpose: tc.purpose, TokenHash: row.TokenHash,
				Now: tokenNow(), NewPasswordHash: tc.newHash,
			})

			require.NoError(t, err)
			assert.Equal(t, id, res.SubjectID)
			assert.Equal(t, tc.payload, res.Payload)
			got, err := ids.ByID(t.Context(), id)
			require.NoError(t, err)
			tc.check(t, got)
		})
	}
}

// ОДНОРАЗОВОСТЬ ПОД ГОНКОЙ: десять горутин на один токен, ровно одна получает
// его. Двойник, гасящий токен без блокировки, отдал бы его всем — и тест
// потребителя на «ссылка одноразовая» был бы зелёным при неработающей
// одноразовости.
func TestMemTokens_ConsumeIsOnceUnderRace(t *testing.T) {
	t.Parallel()

	ids, tokens, id := newTokenStand(t)
	row := tokenRow(id, token.PurposeVerify, "")
	require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{}))

	const racers = 10
	results := make(chan error, racers)
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			start.Wait()
			_, err := tokens.Consume(t.Context(), session.ConsumeRequest{
				Realm: tokenRealm, Purpose: token.PurposeVerify, TokenHash: row.TokenHash, Now: tokenNow(),
			})
			results <- err
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
		require.ErrorIs(t, err, session.ErrTokenInvalid)
	}
	assert.Equal(t, 1, won, "токен обязан достаться ровно одному")

	got, err := ids.ByID(t.Context(), id)
	require.NoError(t, err)
	assert.True(t, got.Verified)
}

// ЭФФЕКТ И ГАШЕНИЕ НЕДЕЛИМЫ: сбой эффекта оставляет токен ЖИВЫМ — как откат
// транзакции в адаптере потребителя. Двойник, гасящий токен до эффекта,
// оставил бы человека и без ссылки, и без смены адреса.
func TestMemTokens_FailedEffectLeavesTokenAlive(t *testing.T) {
	t.Parallel()

	ids, tokens, id := newTokenStand(t)
	_, err := ids.Create(t.Context(), newLogin, "$argon2id$v=19$busy", tokenNow())
	require.NoError(t, err)
	row := tokenRow(id, token.PurposeEmailChange, newLogin)
	require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{}))

	req := session.ConsumeRequest{
		Realm: tokenRealm, Purpose: token.PurposeEmailChange, TokenHash: row.TokenHash, Now: tokenNow(),
	}
	_, err = tokens.Consume(t.Context(), req)

	require.ErrorIs(t, err, auth.ErrLoginTaken)
	assert.Equal(t, 1, tokens.Live(tokenRealm, id, token.PurposeEmailChange), "токен обязан уцелеть")
	got, err := ids.ByID(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, someLogin, got.Login, "логин обязан остаться прежним")
}

// Issue гасит прежние токены того же назначения ТЕМ ЖЕ вызовом: две живые
// ссылки на один аккаунт — два шанса перехвата вместо одного.
func TestMemTokens_IssueRevokesPreviousOfSamePurpose(t *testing.T) {
	t.Parallel()

	_, tokens, id := newTokenStand(t)
	first := tokenRow(id, token.PurposeReset, "")
	second := tokenRow(id, token.PurposeReset, "")
	second.TokenHash = "second" + second.TokenHash[6:]
	other := tokenRow(id, token.PurposeVerify, "")
	require.NoError(t, tokens.Issue(t.Context(), first, session.Notification{}))
	require.NoError(t, tokens.Issue(t.Context(), other, session.Notification{}))

	require.NoError(t, tokens.Issue(t.Context(), second, session.Notification{}))

	assert.Equal(t, 1, tokens.Live(tokenRealm, id, token.PurposeReset))
	assert.Equal(t, 1, tokens.Live(tokenRealm, id, token.PurposeVerify), "чужое назначение не трогается")
	_, err := tokens.Consume(t.Context(), session.ConsumeRequest{
		Realm: tokenRealm, Purpose: token.PurposeReset, TokenHash: first.TokenHash,
		Now: tokenNow(), NewPasswordHash: "$argon2id$v=19$x",
	})
	assert.ErrorIs(t, err, session.ErrTokenInvalid)
}

// Истёкший и погашенный дают ОДИН ответ: «истёк» и «уже использован» вместе
// рассказали бы, что токен был настоящим.
func TestMemTokens_ExpiredAndUsedLookAlike(t *testing.T) {
	t.Parallel()

	_, tokens, id := newTokenStand(t)
	row := tokenRow(id, token.PurposeVerify, "")
	require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{}))

	_, expired := tokens.Consume(t.Context(), session.ConsumeRequest{
		Realm: tokenRealm, Purpose: token.PurposeVerify, TokenHash: row.TokenHash,
		Now: row.ExpiresAt.Add(time.Second),
	})
	_, missing := tokens.Consume(t.Context(), session.ConsumeRequest{
		Realm: tokenRealm, Purpose: token.PurposeVerify, TokenHash: "нет такого", Now: tokenNow(),
	})

	require.ErrorIs(t, expired, session.ErrTokenInvalid)
	require.ErrorIs(t, missing, session.ErrTokenInvalid)
	assert.Equal(t, expired.Error(), missing.Error())
}

// Чужое назначение тем же хэшем не гасится: токен подтверждения, принятый за
// токен сброса, менял бы пароль по ссылке из письма о регистрации.
func TestMemTokens_PurposeIsPartOfTheKey(t *testing.T) {
	t.Parallel()

	_, tokens, id := newTokenStand(t)
	row := tokenRow(id, token.PurposeVerify, "")
	require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{}))

	_, err := tokens.Consume(t.Context(), session.ConsumeRequest{
		Realm: tokenRealm, Purpose: token.PurposeReset, TokenHash: row.TokenHash,
		Now: tokenNow(), NewPasswordHash: "$argon2id$v=19$x",
	})

	assert.ErrorIs(t, err, session.ErrTokenInvalid)
}

// Наружу уходит КОПИЯ: правка полученного не должна менять состояние двойника
// (docs/PATTERNS.md, паттерн 7).
func TestDoubles_HandOutCopies(t *testing.T) {
	t.Parallel()

	_, tokens, id := newTokenStand(t)
	row := tokenRow(id, token.PurposeVerify, "")
	require.NoError(t, tokens.Issue(t.Context(), row, session.Notification{Login: someLogin}))

	issued := tokens.Issued()
	issued[0].Login = "hijacked@example.invalid"
	assert.Equal(t, someLogin, tokens.Issued()[0].Login)

	notifier := authtest.NewRecordingNotifier()
	require.NoError(t, notifier.Notify(t.Context(), session.Notification{Login: someLogin}))
	notes := notifier.Notifications()
	notes[0].Login = "hijacked@example.invalid"
	assert.Equal(t, someLogin, notifier.Notifications()[0].Login)

	auditor := authtest.NewRecordingAuditor()
	require.NoError(t, auditor.Record(t.Context(), session.Event{Kind: session.EventSignedIn}))
	events := auditor.Events()
	events[0].Kind = session.EventLockedOut
	assert.Equal(t, session.EventSignedIn, auditor.Events()[0].Kind)

	attempts := authtest.NewMemAttempts()
	require.NoError(t, attempts.Record(t.Context(), authtest.SuiteAttempt("k", tokenNow())))
	recorded := attempts.Recorded()
	recorded[0].LoginKey = "hijacked"
	assert.Equal(t, "k", attempts.Recorded()[0].LoginKey)
}

// Двойник переживает те же пограничные аргументы, что и адаптер: паника на
// пустом реалме или нулевом идентификаторе выглядела бы у потребителя как
// «сломался ваш пакет».
func TestDoubles_SurviveEdgeArguments(t *testing.T) {
	t.Parallel()

	sessions := authtest.NewMemSessions()
	attempts := authtest.NewMemAttempts()
	_, tokens, _ := newTokenStand(t)

	assert.NotPanics(t, func() {
		_, _ = sessions.DeleteOfSubject(t.Context(), "", uuid.Nil)
		_, _ = sessions.DeleteExpired(t.Context(), "", time.Time{})
		_ = sessions.Delete(t.Context(), "", "")
		_, _ = attempts.Count(t.Context(), "", "", time.Time{})
		_, _ = attempts.Purge(t.Context(), "", time.Time{})
		_, _ = tokens.Revoke(t.Context(), "", uuid.Nil, "", time.Time{})
		_, _ = tokens.PurgeExpired(t.Context(), "", time.Time{})
	})
}

func TestNewMemTokens_PanicsWithoutIdentities(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { authtest.NewMemTokens(nil) })
}

func newTokenStand(t *testing.T) (*authtest.MemIdentities, *authtest.MemTokens, uuid.UUID) {
	t.Helper()
	ids := authtest.NewMemIdentities()
	id, err := ids.Create(t.Context(), someLogin, "$argon2id$v=19$old", tokenNow())
	require.NoError(t, err)
	return ids, authtest.NewMemTokens(ids), id
}

func tokenRow(id uuid.UUID, purpose token.Purpose, payload string) session.OneTimeToken {
	return session.OneTimeToken{
		TokenHash: string(purpose) + "-" + uuid.New().String(),
		Realm:     tokenRealm,
		Purpose:   purpose,
		SubjectID: id,
		Payload:   payload,
		CreatedAt: tokenNow(),
		ExpiresAt: tokenNow().Add(time.Hour),
	}
}
