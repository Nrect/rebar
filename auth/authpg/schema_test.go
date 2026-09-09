package authpg_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Schema — то, что потребитель применит из кода: обязан совпадать с файлом,
// который он же может скопировать в миграции.
func TestSchema_EmbedEqualsFile(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	assert.Equal(t, string(raw), authpg.Schema)
}

// ИМЕНА ОГРАНИЧЕНИЙ И ИНДЕКСОВ — КОНТРАКТ. Они выданы ADR-0003, код разбирает
// конфликт по имени, и переименование ломает потребителя молча.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	up, down, ok := strings.Cut(authpg.Schema, pgtest.GooseDownMarker)
	require.True(t, ok, "в schema.sql нет маркера %s", pgtest.GooseDownMarker)

	for _, want := range []string{
		"CREATE TABLE auth_sessions",
		"CREATE TABLE auth_tokens",
		"CREATE TABLE auth_login_attempts",
		"auth_sessions_idle_chk",
		"ix_auth_sessions_subject",
		"ix_auth_sessions_expires",
		"ix_auth_tokens_subject",
		"ix_auth_tokens_expires",
		"ix_auth_login_attempts_key",
		"ix_auth_login_attempts_at",
	} {
		assert.Contains(t, up, want)
	}
	for _, table := range []string{"auth_sessions", "auth_tokens", "auth_login_attempts"} {
		assert.Contains(t, down, "DROP TABLE "+table)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.NotContains(t, down, "CREATE TABLE")

	// Имя схемы не зашито, FK на таблицы потребителя нет, время приходит
	// параметром: три правила CONVENTIONS §9 разом.
	assert.NotContains(t, authpg.Schema, "public.")
	assert.NotContains(t, authpg.Schema, "REFERENCES",
		"пакет не знает имени таблицы пользователей: FK добавляет потребитель")
	// Проверяется САМ SQL, без строк комментариев: слова «DEFAULT now()» из
	// объяснения в шапке не должны выглядеть как объявление колонки.
	assert.NotContains(t, sqlWithoutComments(authpg.Schema), "DEFAULT now()",
		"время приходит параметром: иначе тесты на управляемых часах проверяют одно, а база пишет другое")
}

// CHECK ⊇ All*: закрытый набор кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestSchemaFile_PurposeCheckMirrorsAllPurposes(t *testing.T) {
	t.Parallel()

	for _, purpose := range token.AllPurposes {
		assert.Containsf(t, authpg.Schema, "'"+purpose.String()+"'",
			"назначение %s объявлено в token.AllPurposes, но не в CHECK колонки purpose", purpose)
	}
	// И наоборот: словарь базы не шире набора кода — значение, которое база
	// принимает, а код не знает, нельзя ни погасить, ни применить.
	check, _, ok := strings.Cut(strings.SplitN(authpg.Schema, "purpose IN (", 2)[1], ")")
	require.True(t, ok)
	assert.Len(t, strings.Split(check, ","), len(token.AllPurposes))
}

// Форма реалма продублирована в CHECK каждой таблицы: реалм входит в ключи
// строк и в WHERE уборки, и разъезд кода с базой даёт строки, которые уже
// нельзя ни прочитать, ни удалить.
func TestSchemaFile_RealmCheckMirrorsRealmForm(t *testing.T) {
	t.Parallel()

	const form = "realm ~ '^[a-z0-9_]{1,32}$'"
	assert.Equal(t, 3, strings.Count(authpg.Schema, form),
		"CHECK формы реалма обязан стоять во всех трёх таблицах")
	assert.Contains(t, form, "1,32", "потолок в схеме разошёлся с auth.MaxRealmLen")
	assert.Equal(t, 32, auth.MaxRealmLen)
}

// Обе стороны миграции применяются на пустую базу: потребитель обязан уметь
// откатиться и накатить заново.
func TestSchemaFile_BothDirectionsApply(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)

	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	require.NoError(t, authpg.New(pool).CheckSchema(t.Context()))

	_, down, ok := strings.Cut(authpg.Schema, pgtest.GooseDownMarker)
	require.True(t, ok)
	pgtest.Apply(t, pool, down)
	require.Error(t, authpg.New(pool).CheckSchema(t.Context()), "после Down таблиц нет")

	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	require.NoError(t, authpg.New(pool).CheckSchema(t.Context()))
}

// CheckSchema СВЕРЯЕТ, НО НЕ ПРИМЕНЯЕТ: автомиграция из библиотеки даёт две
// правды о схеме, требует DDL-прав у приложения и гонку реплик при выкате.
func TestCheckSchema_ReportsAndChangesNothing(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := authpg.New(pool)

	err := store.CheckSchema(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_sessions")
	assert.Contains(t, err.Error(), "schema.sql", "первая строка обязана говорить, что делать")
	// Ничего не создалось: проверка — это проверка. Схема теста своя, поэтому
	// current_schema() отсекает таблицы соседних параллельных тестов.
	assert.Zero(t, countRows(t, pool,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'auth_sessions'`))
}

// sqlWithoutComments — схема без строк комментариев: правила о содержимом
// схемы говорят про SQL, а не про объяснения рядом с ним.
func sqlWithoutComments(schema string) string {
	var out strings.Builder
	for line := range strings.SplitSeq(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
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
