package entitlementpg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

// canary — метка в начале предмета: Postgres обрезает длинные значения в
// Detail, и метка в хвосте туда не доехала бы.
const canary = "leak-canary-4f1d"

// СТРОКА ТАБЛИЦЫ НЕ ПОПАДАЕТ В ОШИБКУ. Проверка ПО ТИПУ: Detail в Error() не
// печатается, он достаётся через errors.As ниже по стеку, и проверка текста
// зелена даже на адаптере, завернувшем *pgconn.PgError целиком.
func TestStore_ErrorHasNoRowData(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	subject := uuid.New()
	oversized := canary + strings.Repeat("x", entitlement.MaxItemIDLen)

	// Сперва — что утекать ЕСТЬ ЧЕМУ: то же нарушение мимо адаптера.
	_, rawErr := pool.Exec(t.Context(),
		`INSERT INTO entitlement_grants (subject_id, item_id, granted_at) VALUES ($1, $2, $3)`,
		subject, oversized, moment())
	var raw *pgconn.PgError
	require.ErrorAs(t, rawErr, &raw, "подготовка: нарушение обязано дать PgError")
	require.Equal(t, "ck_entitlement_grants_item_id", raw.ConstraintName)
	require.Contains(t, raw.Detail, canary, "утекать нечему — тест ничего не сторожит")
	require.Contains(t, raw.Detail, subject.String())

	err := store.Grant(t.Context(), subject, entitlement.Grant{ItemID: oversized}, moment())

	require.ErrorIs(t, err, entitlement.ErrInvalidGrant)
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "ошибка обязана пройти границу postgres.Sanitize")
	assert.Equal(t, "23514", sanitized.Code, "check_violation")
	assert.Equal(t, "ck_entitlement_grants_item_id", sanitized.Constraint, "имя ограничения — не данные")
	var leaked *pgconn.PgError
	require.NotErrorAs(t, err, &leaked, "ошибка драйвера снята с цепочки: в её Detail вся строка")

	// Текст — вторая линия, а не первая.
	assert.NotContains(t, err.Error(), canary)
	assert.NotContains(t, err.Error(), subject.String())
	assert.NotContains(t, err.Error(), "Failing row")
}

// РАЗБОР ПО ИМЕНИ, А НЕ ПО SQLSTATE: CHECK потребителя в той же таблице даёт
// тот же 23514, но это сбой записи, а не негодный предмет.
func TestStore_ConsumerCheckStaysUnavailable(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE entitlement_grants ADD CONSTRAINT consumer_grants_expiry_chk
		CHECK (expires_at IS NULL OR expires_at > granted_at)`)

	past := moment().Add(-time.Hour)
	err := store.Grant(t.Context(), uuid.New(), entitlement.Grant{ItemID: item, ExpiresAt: &past}, moment())

	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	require.NotErrorIs(t, err, entitlement.ErrInvalidGrant)
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized)
	assert.Equal(t, "23514", sanitized.Code, "тот же SQLSTATE, что у потолка предмета")
	assert.Equal(t, "consumer_grants_expiry_chk", sanitized.Constraint)
}

// СБОЙ БАЗЫ — НЕДОСТУПНОСТЬ, А НЕ «НИЧЕГО НЕ КУПЛЕНО»: иначе потребитель
// ответит 403 и утопит инцидент.
func TestStore_FailureIsUnavailable(t *testing.T) {
	t.Parallel()
	store := entitlementpg.New(newSchemaPool(t)) // миграция не применена: таблицы нет
	subject := uuid.New()

	_, err := store.Open(t.Context(), subject, moment())
	requireUnavailable(t, err)
	requireUnavailable(t, store.Grant(t.Context(), subject, entitlement.Grant{ItemID: item}, moment()))
	requireUnavailable(t, store.Revoke(t.Context(), subject, item))
}

// requireUnavailable — сбой базы, и только он: не негодный предмет.
func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	require.NotErrorIs(t, err, entitlement.ErrInvalidGrant, "сбой базы — не негодный предмет")
}
