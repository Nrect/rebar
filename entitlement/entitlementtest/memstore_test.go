package entitlementtest_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementtest"
)

// Тот же набор пойдёт по будущему pg-адаптеру: двойник и адаптер не имеют
// права разойтись, иначе тесты потребителя зелены при сломанном проде.
func TestMemStore_SatisfiesStoreContract(t *testing.T) {
	t.Parallel()

	entitlementtest.RunStoreSuite(t, func(*testing.T) entitlement.Store {
		return entitlementtest.NewMemStore()
	})
}

// Ошибка двойника отличима от доменных: тест не должен принять поломку стенда
// за штатный отказ.
func TestMemStore_ErrIsDistinguishable(t *testing.T) {
	t.Parallel()

	stand := errors.New("стенд лёг")
	store := entitlementtest.NewMemStore()
	store.SetErr(stand)
	subject := uuid.New()

	_, err := store.Open(t.Context(), subject, time.Now())
	require.ErrorIs(t, err, stand)
	require.ErrorIs(t, store.Grant(t.Context(), subject, entitlement.Grant{ItemID: entitlementtest.SuiteItem}), stand)
	require.ErrorIs(t, store.Revoke(t.Context(), subject, entitlementtest.SuiteItem), stand)
	require.NotErrorIs(t, err, entitlement.ErrUnavailable, "ошибка стенда — не доменная ошибка пакета")

	store.SetErr(nil)
	_, err = store.Open(t.Context(), subject, time.Now())
	assert.NoError(t, err)
}

// Двойник переживает те же пограничные аргументы, что и адаптер: паника здесь
// выглядела бы у потребителя как поломка пакета.
func TestMemStore_SurvivesEdgeArguments(t *testing.T) {
	t.Parallel()

	store := entitlementtest.NewMemStore()
	assert.NotPanics(t, func() {
		require.NoError(t, store.Revoke(t.Context(), uuid.Nil, ""))
		require.NoError(t, store.Grant(t.Context(), uuid.Nil, entitlement.Grant{}))
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
			_ = store.Grant(t.Context(), subject, entitlement.Grant{ItemID: entitlementtest.SuiteItem})
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
