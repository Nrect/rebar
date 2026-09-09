package authpg_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

// СОДЕРЖИМОЕ СТРОКИ НЕ ВЫХОДИТ ЗА ГРАНИЦУ АДАПТЕРА.
//
// Проверяется ТИП границы, а не текст ошибки: тест «секрета нет в Error()»
// зелен и на адаптере, который заворачивает *pgconn.PgError целиком — Detail
// в Error() не печатается, он достаётся через errors.As ниже по стеку
// (docs/CHIP.md, «Типовые ошибки чипов»).
//
// Первая половина теста доказывает, что утекать ЕСТЬ ЧЕМУ: то же нарушение
// мимо адаптера несёт ключ сессии прямо в Detail.
func TestStore_ErrorsStopAtTheAdapterBoundary(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	sess := testSession(pgtest.Now())
	require.NoError(t, store.Insert(t.Context(), sess))

	// Мимо адаптера: PgError.Detail несёт «Key (token_hash)=(…) already exists».
	_, rawErr := pool.Exec(t.Context(),
		`INSERT INTO auth_sessions (token_hash, realm, subject_id, created_at, last_seen_at,
			expires_at, idle_expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sess.TokenHash, string(sess.Realm), sess.SubjectID, sess.CreatedAt, sess.LastSeenAt,
		sess.ExpiresAt, sess.IdleExpiresAt)
	var raw *pgconn.PgError
	require.ErrorAs(t, rawErr, &raw, "подготовка теста: нарушение обязано дать PgError")
	require.Contains(t, raw.Detail, sess.TokenHash, "утекать нечему — тест ничего не сторожит")

	// Через адаптер: остаются SQLSTATE, Message и имя ограничения.
	err := store.Insert(t.Context(), sess)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "граница обязана дать postgres.Error")
	assert.Equal(t, "23505", sanitized.Code)
	assert.NotErrorAs(t, err, &raw, "*pgconn.PgError не должен доезжать до вызывающего")
	assert.NotContains(t, err.Error(), sess.TokenHash)
}

// Своей копии границы у адаптера нет: пять копий postgres.Sanitize — пять
// шансов разойтись ровно там, где расхождение стоит утечки строки.
func TestStore_UsesTheSharedSanitizeBoundary(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	bad := testSession(pgtest.Now(), func(s *session.Session) { s.Realm = "Shop" })

	err := store.Insert(t.Context(), bad)

	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "23514", sanitized.Code, "нарушение CHECK обязано доехать классифицируемым")
}

// assertConstraint — нарушение ИМЕННО этого ограничения. Имя — контракт схемы,
// а не деталь реализации: код разбирает конфликт по нему.
func assertConstraint(t *testing.T, err error, constraint string) {
	t.Helper()
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, constraint, sanitized.Constraint)
}
