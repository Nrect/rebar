package authzpg_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
)

// Выдача и чтение: роли возвращаются по порядку, реалмы разделены.
func TestStore_AssignAndRolesOf(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	mustAssign(t, store, grant(staff("u1"), "viewer"))
	mustAssign(t, store, grant(staff("u1"), "clerk"))
	mustAssign(t, store, grant(authz.Subject{Realm: "customers", ID: "u1"}, "buyer"))

	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"clerk", "viewer"}, roles)

	roles, err = store.RolesOf(t.Context(), authz.Subject{Realm: "customers", ID: "u1"})
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"buyer"}, roles, "реалмы не видят ролей друг друга")

	roles, err = store.RolesOf(t.Context(), staff("u2"))
	require.NoError(t, err)
	assert.Empty(t, roles, "неизвестный субъект — пустой список, а не сбой")
}

// ИСТЁКШЕЕ НАЗНАЧЕНИЕ НЕ ДАЁТ ПРАВ С МОМЕНТА ИСТЕЧЕНИЯ, а не с момента
// уборки: иначе доступ «залипал» бы до ближайшего прогона PurgeExpired.
func TestStore_RolesOf_SkipsExpired(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	at := testNow()
	until := at.Add(time.Hour)
	mustAssign(t, store, grant(staff("u1"), "viewer", func(a *authzpg.Assignment) {
		a.GrantedAt, a.ExpiresAt = at, &until
	}))
	mustAssign(t, store, grant(staff("u1"), "clerk", func(a *authzpg.Assignment) { a.GrantedAt = at }))

	store.SetClock(func() time.Time { return until.Add(-time.Minute) })
	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"clerk", "viewer"}, roles)

	store.SetClock(func() time.Time { return until })
	roles, err = store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"clerk"}, roles, "в момент истечения роль уже не действует")
}

// Повторная выдача переписывает срок и того, кто выдал: «продлить» и
// «выдать» — одно действие, и различать их значило бы требовать от
// потребителя знать, была ли роль раньше.
func TestStore_AssignIsUpsert(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	at := testNow()
	first := at.Add(time.Hour)
	mustAssign(t, store, grant(staff("u1"), "viewer", func(a *authzpg.Assignment) {
		a.GrantedAt, a.ExpiresAt, a.GrantedBy = at, &first, "admin-1"
	}))

	second := at.Add(72 * time.Hour)
	mustAssign(t, store, grant(staff("u1"), "viewer", func(a *authzpg.Assignment) {
		a.GrantedAt, a.ExpiresAt, a.GrantedBy = at, &second, "admin-2"
	}))

	assert.Equal(t, 1, countRows(t, pool))

	list, err := store.ListOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "admin-2", list[0].GrantedBy)
	require.NotNil(t, list[0].ExpiresAt)
	assert.Equal(t, second, *list[0].ExpiresAt)
}

// Отзыв повторяют: второй вызов не сбой, а «такого назначения нет».
func TestStore_Revoke(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	mustAssign(t, store, grant(staff("u1"), "viewer"))

	revoked, err := store.Revoke(t.Context(), staff("u1"), "viewer")
	require.NoError(t, err)
	assert.True(t, revoked)

	revoked, err = store.Revoke(t.Context(), staff("u1"), "viewer")
	require.NoError(t, err)
	assert.False(t, revoked)

	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Empty(t, roles)
}

// Снятие всех ролей — увольнение и блокировка; чужих ролей не трогает.
func TestStore_RevokeAll(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	mustAssign(t, store, grant(staff("u1"), "viewer"))
	mustAssign(t, store, grant(staff("u1"), "clerk"))
	mustAssign(t, store, grant(staff("u2"), "viewer"))

	n, err := store.RevokeAll(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	roles, err := store.RolesOf(t.Context(), staff("u2"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"viewer"}, roles)

	n, err = store.RevokeAll(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Витрина оператора показывает и истёкшие: «роль была до пятницы» ему нужно
// видеть. Решения по правам принимает RolesOf, а не этот список.
func TestStore_ListOfIncludesExpired(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	at := testNow()
	until := at.Add(time.Hour)
	mustAssign(t, store, grant(staff("u1"), "viewer", func(a *authzpg.Assignment) {
		a.GrantedAt, a.ExpiresAt = at, &until
	}))
	store.SetClock(func() time.Time { return until.Add(time.Hour) })

	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Empty(t, roles)

	list, err := store.ListOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, authz.Role("viewer"), list[0].Role)
	assert.Equal(t, staff("u1"), list[0].Subject)
	assert.Equal(t, at, list[0].GrantedAt)
	require.NotNil(t, list[0].ExpiresAt)
	assert.Equal(t, until, *list[0].ExpiresAt)
}

// Уборка не трогает живые назначения и уважает потолок пачки.
func TestStore_PurgeExpired(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	at := testNow()
	for _, role := range []authz.Role{"viewer", "clerk", "manager"} {
		until := at.Add(time.Hour)
		mustAssign(t, store, grant(staff("u1"), role, func(a *authzpg.Assignment) {
			a.GrantedAt, a.ExpiresAt = at, &until
		}))
	}
	mustAssign(t, store, grant(staff("u1"), "owner", func(a *authzpg.Assignment) { a.GrantedAt = at }))

	before := at.Add(2 * time.Hour)
	n, err := store.PurgeExpired(t.Context(), before, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "потолок пачки уважается")

	n, err = store.PurgeExpired(t.Context(), before, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	assert.Equal(t, 1, countRows(t, pool),
		"бессрочное назначение уборка не трогает")

	n, err = store.PurgeExpired(t.Context(), before, 0)
	require.NoError(t, err)
	assert.Zero(t, n, "непозитивный потолок — ноль удалённых без ошибки")
}

// Уборка не забирает назначение, срок которого ещё не наступил.
func TestStore_PurgeExpired_KeepsLive(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	at := testNow()
	until := at.Add(time.Hour)
	mustAssign(t, store, grant(staff("u1"), "viewer", func(a *authzpg.Assignment) {
		a.GrantedAt, a.ExpiresAt = at, &until
	}))

	n, err := store.PurgeExpired(t.Context(), at.Add(time.Minute), 10)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, countRows(t, pool))
}

// КОНТРАКТ «В ОДНОЙ ТРАНЗАКЦИИ» доказывается чтением ПОСЛЕ ОТКАТА: адаптер,
// открывающий своё соединение, оставил бы роль выданной после отката
// бизнес-факта — сотрудник уволен, а права остались.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.WithTx(tx).Assign(t.Context(), grant(staff("u1"), "viewer")))
	require.NoError(t, tx.Rollback(t.Context()))

	assert.Zero(t, countRows(t, pool),
		"после отката роли остаться не должно")

	tx, err = pool.Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.WithTx(tx).Assign(t.Context(), grant(staff("u1"), "viewer")))
	require.NoError(t, tx.Commit(t.Context()))

	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"viewer"}, roles)
}

// Отзыв в транзакции откатывается вместе с бизнес-фактом.
func TestStore_WithTx_RevokeIsAtomic(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	mustAssign(t, store, grant(staff("u1"), "viewer"))

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	revoked, err := store.WithTx(tx).Revoke(t.Context(), staff("u1"), "viewer")
	require.NoError(t, err)
	require.True(t, revoked)
	require.NoError(t, tx.Rollback(t.Context()))

	roles, err := store.RolesOf(t.Context(), staff("u1"))
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"viewer"}, roles, "откат вернул роль")
}

// СБОЙ БАЗЫ — НЕДОСТУПНОСТЬ, А НЕ «РОЛЕЙ НЕТ»: иначе потребитель ответит 403
// и утопит инцидент.
func TestStore_FailureIsUnavailable(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t) // миграция не применена: таблицы нет
	store := authzpg.New(pool)

	_, err := store.RolesOf(t.Context(), staff("u1"))
	require.ErrorIs(t, err, authz.ErrUnavailable)

	require.ErrorIs(t, store.Assign(t.Context(), grant(staff("u1"), "viewer")), authz.ErrUnavailable)

	_, err = store.Revoke(t.Context(), staff("u1"), "viewer")
	require.ErrorIs(t, err, authz.ErrUnavailable)

	_, err = store.RevokeAll(t.Context(), staff("u1"))
	require.ErrorIs(t, err, authz.ErrUnavailable)

	_, err = store.ListOf(t.Context(), staff("u1"))
	require.ErrorIs(t, err, authz.ErrUnavailable)

	_, err = store.PurgeExpired(t.Context(), testNow(), 10)
	require.ErrorIs(t, err, authz.ErrUnavailable)
}

// СТРОКА ТАБЛИЦЫ НЕ ПОПАДАЕТ В ОШИБКУ: в Detail Postgres кладёт «Failing row
// contains (…)» вместе с идентификатором субъекта и тем, кто выдал роль.
func TestStore_ErrorHasNoRowData(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	const canary = "subject-must-not-leak"
	err := store.Assign(t.Context(), grant(authz.Subject{Realm: "staff", ID: canary}, "Manager"))

	require.ErrorIs(t, err, authz.ErrUnavailable, "нарушение CHECK — сбой записи")
	assert.NotContains(t, err.Error(), canary)
	assert.NotContains(t, err.Error(), "Failing row")
	assert.NotContains(t, err.Error(), "admin-1")
	assert.Contains(t, err.Error(), "authz_role_assignments_role_chk", "имя ограничения — не данные")
}

// Аноним до базы не доходит: у субъекта, которого нет, не может быть ролей.
func TestStore_AnonymousSubject(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	roles, err := store.RolesOf(t.Context(), authz.Subject{})
	require.NoError(t, err)
	assert.Empty(t, roles)

	list, err := store.ListOf(t.Context(), authz.Subject{})
	require.NoError(t, err)
	assert.Empty(t, list)

	revoked, err := store.Revoke(t.Context(), authz.Subject{}, "viewer")
	require.NoError(t, err)
	assert.False(t, revoked)

	n, err := store.RevokeAll(t.Context(), authz.Subject{})
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Негодное назначение отвергается ДО запроса: чинить это в коде потребителя,
// а не в базе.
func TestStore_AssignRejectsInvalid(t *testing.T) {
	t.Parallel()

	at := testNow()
	before := at.Add(-time.Hour)
	tests := []struct {
		name string
		a    authzpg.Assignment
	}{
		{name: "аноним", a: authzpg.Assignment{Role: "viewer", GrantedAt: at}},
		{name: "пустая роль", a: authzpg.Assignment{Subject: staff("u1"), GrantedAt: at}},
		{name: "без времени выдачи", a: authzpg.Assignment{Subject: staff("u1"), Role: "viewer"}},
		{
			name: "срок раньше выдачи",
			a:    authzpg.Assignment{Subject: staff("u1"), Role: "viewer", GrantedAt: at, ExpiresAt: &before},
		},
		{
			name: "срок равен выдаче",
			a:    authzpg.Assignment{Subject: staff("u1"), Role: "viewer", GrantedAt: at, ExpiresAt: &at},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, _ := newStore(t)
			err := store.Assign(t.Context(), tt.a)
			require.ErrorIs(t, err, authzpg.ErrInvalidAssignment)
			assert.NotErrorIs(t, err, authz.ErrUnavailable, "ошибка данных — не сбой хранилища")
		})
	}
}

// Конструкторы паникуют на нулевых аргументах: ошибка проводки падает на
// старте процесса.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "authzpg.New: nil pool", func() { authzpg.New(nil) })
	assert.PanicsWithValue(t, "authzpg.WithTx: nil tx", func() {
		var tx pgx.Tx
		new(authzpg.Store).WithTx(tx)
	})
}

// Store реализует порт ядра: расхождение сигнатур ловится компилятором.
var _ authz.RoleSource = (*authzpg.Store)(nil)
