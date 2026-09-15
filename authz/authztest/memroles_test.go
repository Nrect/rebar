package authztest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
	"github.com/nrect/rebar/kit/errs"
)

var subj = authz.Subject{Realm: "staff", ID: "u1"}

// Двойник ведёт себя как источник ролей: назначение, отзыв, неизвестный
// субъект без ролей и без ошибки.
func TestMemRoles_Assignment(t *testing.T) {
	t.Parallel()

	src := authztest.NewMemRoles()

	roles, err := src.RolesOf(t.Context(), subj)
	require.NoError(t, err, "неизвестный субъект — отказ по правилу, а не сбой")
	assert.Empty(t, roles)

	src.Set(subj, "viewer", "clerk")
	roles, err = src.RolesOf(t.Context(), subj)
	require.NoError(t, err)
	assert.Equal(t, []authz.Role{"viewer", "clerk"}, roles)

	src.Set(subj, "viewer")
	roles, _ = src.RolesOf(t.Context(), subj)
	assert.Equal(t, []authz.Role{"viewer"}, roles, "Set заменяет, а не дополняет")

	src.Add(subj, "clerk", "clerk")
	roles, _ = src.RolesOf(t.Context(), subj)
	assert.Equal(t, []authz.Role{"viewer", "clerk"}, roles, "Add не удваивает роль")

	src.Remove(subj, "viewer")
	roles, _ = src.RolesOf(t.Context(), subj)
	assert.Equal(t, []authz.Role{"clerk"}, roles)
}

// Двойник не паникует на пограничных аргументах, которые адаптер переживает:
// паника двойника маскируется под ошибку теста потребителя.
func TestMemRoles_SurvivesEdgeArguments(t *testing.T) {
	t.Parallel()

	src := authztest.NewMemRoles()

	assert.NotPanics(t, func() { src.Remove(subj, "missing") })
	assert.NotPanics(t, func() { src.Set(subj) })
	assert.NotPanics(t, func() { src.Add(subj) })

	roles, err := src.RolesOf(t.Context(), authz.Subject{})
	require.NoError(t, err, "аноним — пустой список, а не сбой")
	assert.Empty(t, roles)
}

// Снимок — копия: правка возвращённого среза не меняет хранилище.
func TestMemRoles_ReturnsCopy(t *testing.T) {
	t.Parallel()

	src := authztest.NewMemRoles()
	src.Set(subj, "viewer")

	roles, err := src.RolesOf(t.Context(), subj)
	require.NoError(t, err)
	roles[0] = "manager"

	again, _ := src.RolesOf(t.Context(), subj)
	assert.Equal(t, []authz.Role{"viewer"}, again)
}

// Заданный сбой источника приходит так, как его отдаёт authzpg: класс 503,
// authz.ErrUnavailable и причина в одной цепочке. Голая причина давала бы
// потребителю, зовущему источник мимо Authorizer, 500 там, где прод отвечает
// 503.
func TestMemRoles_InjectedErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	src := authztest.NewMemRoles()
	src.Set(subj, "viewer")
	src.SetErr(down)

	_, err := src.RolesOf(t.Context(), subj)
	requireUnavailable(t, err, down)
}

// Отменённый контекст — отказ, как у настоящего хранилища, и в том же виде:
// authz.ErrUnavailable с context.Canceled в цепочке.
func TestMemRoles_CanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := authztest.NewMemRoles().RolesOf(ctx, subj)
	requireUnavailable(t, err, context.Canceled)
}

// Аноним не падает ни на сбое, ни на отмене: authzpg отвечает ему nil без
// похода в хранилище. Двойник, падающий здесь, изображал бы сбой, которого в
// проде нет.
func TestMemRoles_AnonymousIgnoresInjectedError(t *testing.T) {
	t.Parallel()

	src := authztest.NewMemRoles()
	src.SetErr(errors.New("connection refused"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	roles, err := src.RolesOf(ctx, authz.Subject{})
	require.NoError(t, err)
	assert.Empty(t, roles)
}

// requireUnavailable — все три стороны сразу: класс, sentinel модуля и
// причина. Проверка одной чинила бы её ценой другой.
func requireUnavailable(t *testing.T, err, cause error) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки: %v", err)
	require.ErrorIs(t, err, authz.ErrUnavailable)
	require.ErrorIs(t, err, cause)
}

// Двойник потокобезопасен: тесты потребителя идут под -race.
func TestMemRoles_Race(t *testing.T) {
	t.Parallel()

	src := authztest.NewMemRoles()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 200 {
				switch n % 4 {
				case 0:
					src.Set(subj, "viewer")
				case 1:
					src.Add(subj, "clerk")
				case 2:
					src.Remove(subj, "clerk")
				default:
					if _, err := src.RolesOf(t.Context(), subj); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}
