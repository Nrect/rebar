package authpg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/postgres/pgtest"
)

// CheckSchema СВЕРЯЕТ, НО НЕ ПРИМЕНЯЕТ: автомиграция из библиотеки даёт две
// правды о схеме, требует DDL-прав у приложения и гонку реплик при выкате.
func TestCheckSchema_ReportsAndChangesNothing(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := authpg.New(pool)

	err := store.CheckSchema(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_sessions")
	assert.Contains(t, err.Error(), "накатите authpg.Migrations() раннером проекта",
		"первая строка обязана говорить, что делать")
	// Ничего не создалось: проверка — это проверка. Схема теста своя, поэтому
	// current_schema() отсекает таблицы соседних параллельных тестов.
	assert.Zero(t, countRows(t, pool,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'auth_sessions'`))
}

// Расхождение называется поимённо и всё сразу: чинить схему по одной находке
// за прогон — это столько выкатов, сколько расхождений.
func TestCheckSchema_NamesEveryMismatch(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	pgtest.Apply(t, pool, `DROP INDEX ix_auth_tokens_expires;
		ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_idle_chk;
		ALTER TABLE auth_login_attempts DROP COLUMN login_key`)

	err := store.CheckSchema(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "накатите authpg.Migrations() раннером проекта",
		"первая строка обязана говорить, что делать")
	for _, want := range []string{
		"ix_auth_tokens_expires", "auth_sessions_idle_chk", "login_key",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

// Лишние колонки потребителя расхождением не считаются: его таблица — его
// дело, пока в ней есть всё, на что опирается адаптер.
func TestCheckSchema_IgnoresExtraColumns(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE auth_sessions ADD COLUMN device_label TEXT`)

	assert.NoError(t, store.CheckSchema(t.Context()))
}

// Скользящий срок за абсолютным база не принимает: инвариант держит CHECK, а
// не только зажим в сервисе.
func TestSchema_IdleCheckRefusesSlidingBeyondAbsolute(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	now := pgtest.Now()
	bad := testSession(now, func(s *session.Session) {
		s.IdleExpiresAt = s.ExpiresAt.Add(time.Second)
	})

	err := store.Insert(t.Context(), bad)

	require.ErrorIs(t, err, auth.ErrUnavailable)
	assertConstraint(t, err, "auth_sessions_idle_chk")
}

// User-Agent длиннее потолка база не принимает: сервис его обрезает, а CHECK
// сторожит тех, кто пишет в таблицу мимо сервиса.
func TestSchema_UserAgentCeiling(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	bad := testSession(pgtest.Now(), func(s *session.Session) {
		s.UserAgent = strings.Repeat("x", 255)
	})

	err := store.Insert(t.Context(), bad)

	require.ErrorIs(t, err, auth.ErrUnavailable)
}

// Реалм не той формы база не принимает ни в одной из трёх таблиц.
func TestSchema_RealmCheckRefusesBadRealm(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	bad := testSession(pgtest.Now(), func(s *session.Session) { s.Realm = "Shop" })

	err := store.Insert(t.Context(), bad)

	require.ErrorIs(t, err, auth.ErrUnavailable)
}
