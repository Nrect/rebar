package entitlementtest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementtest"
	"github.com/nrect/rebar/kit/errs"
)

// Тот же набор гоняется по entitlementpg: двойник и адаптер не имеют права
// разойтись, иначе тесты потребителя зелены при сломанном проде.
func TestMemStore_SatisfiesStoreContract(t *testing.T) {
	t.Parallel()

	entitlementtest.RunStoreSuite(t, func(*testing.T) entitlement.Store {
		return entitlementtest.NewMemStore()
	})
}

// Заданный сбой приходит так, как его отдаёт entitlementpg: класс 503,
// entitlement.ErrUnavailable и причина в одной цепочке. Голая причина давала
// бы потребителю, пишущему выдачу мимо сервиса, 500 там, где прод отвечает
// 503; поломку стенда по-прежнему отличает своя причина.
func TestMemStore_InjectedErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	stand := errors.New("стенд лёг")
	store := entitlementtest.NewMemStore()
	store.SetErr(stand)
	subject := uuid.New()

	_, err := store.Open(t.Context(), subject, time.Now())
	requireUnavailable(t, err, stand, "Open")
	requireUnavailable(t, store.Grant(t.Context(), subject,
		entitlement.Grant{ItemID: entitlementtest.SuiteItem}, entitlementtest.SuiteNow()), stand, "Grant")
	requireUnavailable(t, store.Revoke(t.Context(), subject, entitlementtest.SuiteItem), stand, "Revoke")

	store.SetErr(nil)
	_, err = store.Open(t.Context(), subject, time.Now())
	assert.NoError(t, err)
}

// Отменённый контекст приходит так же, как от entitlementpg: в
// entitlement.ErrUnavailable с context.Canceled в цепочке — и на входе в
// метод, и в ожидании Hold.
func TestMemStore_CancelledContextIsUnavailable(t *testing.T) {
	t.Parallel()

	store := entitlementtest.NewMemStore()
	subject := uuid.New()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := store.Open(ctx, subject, entitlementtest.SuiteNow())
	requireUnavailable(t, err, context.Canceled, "Open")
	requireUnavailable(t, store.Grant(ctx, subject,
		entitlement.Grant{ItemID: entitlementtest.SuiteItem}, entitlementtest.SuiteNow()), context.Canceled, "Grant")
	requireUnavailable(t, store.Revoke(ctx, subject, entitlementtest.SuiteItem), context.Canceled, "Revoke")

	store.Hold()
	defer store.Release()
	_, err = store.Open(ctx, subject, entitlementtest.SuiteNow())
	requireUnavailable(t, err, context.Canceled, "Open в ожидании Hold")
}

// requireUnavailable — все три стороны сразу: класс, sentinel модуля и
// причина. Проверка одной чинила бы её ценой другой.
func requireUnavailable(t *testing.T, err, cause error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	require.ErrorIsf(t, err, entitlement.ErrUnavailable, "entitlement.ErrUnavailable на %s", site)
	require.ErrorIsf(t, err, cause, "причина на %s", site)
}

// Двойник переживает те же пограничные аргументы, что и адаптер: паника здесь
// выглядела бы у потребителя как поломка пакета.
func TestMemStore_SurvivesEdgeArguments(t *testing.T) {
	t.Parallel()

	store := entitlementtest.NewMemStore()
	assert.NotPanics(t, func() {
		require.NoError(t, store.Revoke(t.Context(), uuid.Nil, ""))
		require.ErrorIs(t, store.Grant(t.Context(), uuid.Nil, entitlement.Grant{}, time.Time{}),
			entitlement.ErrInvalidGrant, "пустой предмет отвергается, как CHECK у базы, а не принимается молча")
		_, err := store.Open(t.Context(), uuid.Nil, time.Time{})
		require.NoError(t, err)
	})
}

func TestMemStore_Race(t *testing.T) {
	t.Parallel()

	const workers = 16
	store := entitlementtest.NewMemStore()
	subject := uuid.New()

	var wg sync.WaitGroup
	wg.Add(workers * 3)
	for range workers {
		go func() {
			defer wg.Done()
			_ = store.Grant(t.Context(), subject,
				entitlement.Grant{ItemID: entitlementtest.SuiteItem}, entitlementtest.SuiteNow())
		}()
		go func() {
			defer wg.Done()
			_, _ = store.Open(t.Context(), subject, entitlementtest.SuiteNow())
		}()
		go func() {
			defer wg.Done()
			_ = store.Revoke(t.Context(), subject, entitlementtest.SuiteItem)
		}()
	}
	wg.Wait()
	assert.Positive(t, store.Opens())
}

// Набор без фабрики — ошибка проводки теста, и она обязана быть громкой:
// молча пропущенный контрактный набор выглядит как пройденный.
func TestRunStoreSuite_PanicsWithoutFactory(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "entitlementtest.RunStoreSuite: newStore must not be nil", func() {
		entitlementtest.RunStoreSuite(t, nil)
	})
}
