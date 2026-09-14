package entitlementpg_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/entitlement/entitlementtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// ГЛАВНЫЙ ГЕЙТ: контрактный набор порта — тот же, что гоняется по двойнику.
// Разойдись они, и тесты потребителя, написанные на двойнике, зелены при
// сломанном проде (CONVENTIONS §5).
func TestStore_SatisfiesStoreContract(t *testing.T) {
	t.Parallel()

	entitlementtest.RunStoreSuite(t, func(t *testing.T) entitlement.Store {
		t.Helper()
		store, _ := newStore(t)
		return store
	})
}

// Выдач нет — пустой срез, а не nil: так отдаёт двойник, и сравнение с
// []entitlement.Grant{} у потребителя не разойдётся с продом.
func TestStore_Open_EmptyIsNotNil(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	got, err := store.Open(t.Context(), uuid.New(), moment())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got)
}

// ПОРЯДОК ПОБАЙТНЫЙ, как у двойника, при любой сортировке базы потребителя:
// под лингвистической сортировкой «a» шла бы раньше «B», у двойника — наоборот.
func TestStore_Open_OrdersItemsBytewise(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE entitlement_grants ALTER COLUMN item_id TYPE text COLLATE "und-x-icu"`)

	subject := uuid.New()
	for _, itemID := range []string{"a", "B"} {
		require.NoError(t, store.Grant(t.Context(), subject, entitlement.Grant{ItemID: itemID}, moment()))
	}
	got, err := store.Open(t.Context(), subject, moment())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"B", "a"}, []string{got[0].ItemID, got[1].ItemID})
}

// КОНТРАКТ «В ОДНОЙ ТРАНЗАКЦИИ» доказывается чтением ИЗ БАЗЫ ПОСЛЕ ОТКАТА:
// адаптер со своим соединением оставил бы доступ открытым после отката оплаты.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	subject := uuid.New()

	tx := beginTx(t, pool)
	inTx := store.WithTx(tx)
	require.NoError(t, inTx.Grant(t.Context(), subject, entitlement.Grant{ItemID: item}, moment()))

	seen, err := inTx.Open(t.Context(), subject, moment())
	require.NoError(t, err)
	require.Len(t, seen, 1, "в своей транзакции выдача видна")
	require.Zero(t, countRows(t, pool), "до фиксации снаружи не видна: запись шла в транзакции, а не мимо неё")

	require.NoError(t, tx.Rollback(t.Context()))
	assert.Zero(t, countRows(t, pool), "после отката выдачи остаться не должно")
}

// Отзыв в транзакции откатывается вместе с бизнес-фактом.
func TestStore_WithTx_RevokeIsAtomic(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	subject := uuid.New()
	require.NoError(t, store.Grant(t.Context(), subject, entitlement.Grant{ItemID: item}, moment()))

	tx := beginTx(t, pool)
	require.NoError(t, store.WithTx(tx).Revoke(t.Context(), subject, item))
	require.NoError(t, tx.Rollback(t.Context()))

	assert.Equal(t, 1, countRows(t, pool), "откат вернул выдачу")
}

// ПОВТОР ПОД ГОНКОЙ НЕ ВСПЛЫВАЕТ 23505: ON CONFLICT, а не «прочитать и
// вставить», — иначе две параллельные оплаты одного предмета роняли бы
// транзакцию потребителя.
func TestStore_Grant_Race(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	subject := uuid.New()

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			expires := moment().Add(time.Duration(i+1) * time.Hour)
			errs <- store.Grant(t.Context(), subject, entitlement.Grant{ItemID: item, ExpiresAt: &expires}, moment())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, countRows(t, pool))

	// Срок под гонкой тоже не сокращается: остаётся поздний из восьми, в каком
	// бы порядке ни легли записи.
	got, err := store.Open(t.Context(), subject, moment())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ExpiresAt)
	assert.Equal(t, moment().Add(workers*time.Hour), *got[0].ExpiresAt)
}

// ПОТОЛОК РЕЖЕТСЯ ПО БАЙТАМ, как len в ядре: length() по символам пропустил бы
// предмет в 65 знаков и 129 байт, который ядро отвергает.
func TestStore_Grant_ItemCeiling(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	subject := uuid.New()

	fits := strings.Repeat("я", entitlement.MaxItemIDLen/2)
	require.Len(t, fits, entitlement.MaxItemIDLen, "ровно потолок в байтах")
	require.NoError(t, store.Grant(t.Context(), subject, entitlement.Grant{ItemID: fits}, moment()))

	for _, tt := range []struct{ name, itemID string }{
		{name: "пустой", itemID: ""},
		{name: "байтом больше потолка", itemID: fits + "x"},
	} {
		err := store.Grant(t.Context(), subject, entitlement.Grant{ItemID: tt.itemID}, moment())
		require.ErrorIs(t, err, entitlement.ErrInvalidGrant, tt.name)
		require.NotErrorIs(t, err, entitlement.ErrUnavailable, "%s: база ответила определённо, это не сбой", tt.name)
	}
	assert.Equal(t, 1, countRows(t, pool))
}

// Адаптер под сервисом ядра — так его и собирает потребитель: выдача
// открывает, отзыв закрывает сразу.
func TestStore_UnderService(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	svc := entitlement.New(store, entitlement.Config{TTL: time.Minute, MaxSubjects: 8, LoadTimeout: 5 * time.Second})
	subject := uuid.New()

	require.NoError(t, svc.Grant(t.Context(), subject, entitlement.Grant{ItemID: item}))
	require.NoError(t, svc.Require(t.Context(), subject, item))

	require.NoError(t, svc.Revoke(t.Context(), subject, item))
	require.ErrorIs(t, svc.Require(t.Context(), subject, item), entitlement.ErrDenied)
}

// Конструкторы паникуют на нулевых аргументах: ошибка проводки падает на
// старте процесса.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "entitlementpg.New: nil pool", func() { entitlementpg.New(nil) })
	assert.PanicsWithValue(t, "entitlementpg.WithTx: nil tx", func() {
		var tx pgx.Tx
		new(entitlementpg.Store).WithTx(tx)
	})
}
