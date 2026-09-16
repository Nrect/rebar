package ledgertest_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

var errDown = errors.New("ledgertest_test: store is down")

func book() ledger.Book {
	return ledger.Book{
		Name: "wallet", Unit: "RUB",
		Kinds: []ledger.KindSpec{{Name: "topup", Sign: ledger.SignCredit, Reference: ledger.Optional, Attribution: ledger.Optional}},
	}
}

func service(t *testing.T, store ledger.Store) *ledger.Service {
	t.Helper()
	key, err := secrets.GenerateKey()
	require.NoError(t, err)
	return ledger.NewService(store, ledger.Config{Book: book(), Keys: map[secrets.KeyID][]byte{1: key}, ActiveKey: 1})
}

func topup(account uuid.UUID, key string) ledger.PostRequest {
	return ledger.PostRequest{Account: account, Kind: "topup", AmountMinor: 100, IdempotencyKey: key}
}

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{reflect.TypeFor[ledgertest.MemStore](), reflect.TypeFor[ledgertest.Clock]()} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

func TestNewMemStore_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "ledgertest.NewMemStore: at least one book must be registered", func() { ledgertest.NewMemStore() })
	bad := book()
	bad.Unit = ""
	assert.PanicsWithValue(t, "ledgertest.NewMemStore: Book.Unit must match [A-Za-z0-9_]{1,16}", func() { ledgertest.NewMemStore(bad) })
	assert.PanicsWithValue(t, `ledgertest.NewMemStore: book "wallet" is registered twice`, func() { ledgertest.NewMemStore(book(), book()) })
}

// Книга копируется при заведении: правка реестра вызывающим не меняет
// «справочник» хранилища.
func TestNewMemStore_CopiesBooks(t *testing.T) {
	t.Parallel()

	registered := book()
	store := ledgertest.NewMemStore(registered)
	registered.Kinds[0].Sign = ledger.SignDebit

	_, err := service(t, store).Post(t.Context(), topup(uuid.New(), "k"))
	require.NoError(t, err)
}

// Заданный сбой приходит из каждого метода так, как его отдаёт адаптер: класс
// 503, ledger.ErrUnavailable и причина в одной цепочке. Снимается nil.
func TestMemStore_SetErr(t *testing.T) {
	t.Parallel()

	store := ledgertest.NewMemStore(book())
	account := uuid.New()
	require.NoError(t, store.Post(t.Context(), "wallet", account, func(ledger.AccountTx, ledger.Account) error {
		return nil
	}), "контроль: без сбоя Post проходит")

	store.SetErr(errDown)
	err := store.Post(t.Context(), "wallet", account, func(ledger.AccountTx, ledger.Account) error { return nil })
	requireUnavailable(t, err, errDown, "Post")
	_, err = store.Account(t.Context(), "wallet", account)
	requireUnavailable(t, err, errDown, "Account")
	_, err = store.Entries(t.Context(), "wallet", account, 0, 10)
	requireUnavailable(t, err, errDown, "Entries")

	store.SetErr(nil)
	_, err = service(t, store).Post(t.Context(), topup(account, "k"))
	require.NoError(t, err, "nil снимает сбой")
}

// Сбой, выставленный посреди транзакции, видят и методы счёта.
func TestMemStore_AccountTxFailsLikeAdapter(t *testing.T) {
	t.Parallel()

	store := ledgertest.NewMemStore(book())
	account := uuid.New()
	ctx, cancel := context.WithCancel(t.Context())
	err := store.Post(ctx, "wallet", account, func(tx ledger.AccountTx, _ ledger.Account) error {
		cancel()
		_, _, err := tx.EntryByKey(ctx, "k")
		requireUnavailable(t, err, context.Canceled, "EntryByKey")
		_, _, err = tx.EntryByID(ctx, uuid.New())
		requireUnavailable(t, err, context.Canceled, "EntryByID")
		_, _, err = tx.ReversalOf(ctx, uuid.New())
		requireUnavailable(t, err, context.Canceled, "ReversalOf")
		requireUnavailable(t, tx.Insert(ctx, ledger.Entry{}), context.Canceled, "Insert")
		return nil
	})
	requireUnavailable(t, err, context.Canceled, "фиксация")
}

func TestMemStore_UnknownBook(t *testing.T) {
	t.Parallel()

	store := ledgertest.NewMemStore(book())
	called := false
	err := store.Post(t.Context(), "points", uuid.New(), func(ledger.AccountTx, ledger.Account) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ledgertest.ErrUnknownBook)
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	assert.False(t, called)
	assert.Equal(t, errs.KindUnknown, errs.KindOf(err), "книги нет — дефект сборки, а не сбой")

	head, err := store.Account(t.Context(), "points", uuid.New())
	require.NoError(t, err, "чтение неизвестной книги у адаптера — пустая выборка")
	assert.Zero(t, head.Seq)
}

// Счётчик вызовов считает методы порта и методы счёта — основа утверждений
// «до хранилища не дошли».
func TestMemStore_CallCount(t *testing.T) {
	t.Parallel()

	store := ledgertest.NewMemStore(book())
	svc := service(t, store)
	account := uuid.New()
	_, err := svc.Post(t.Context(), topup(account, "k"))
	require.NoError(t, err)
	_, err = svc.Balance(t.Context(), account)
	require.NoError(t, err)
	_, err = svc.Verify(t.Context(), account, ledger.Position{}, 10)
	require.NoError(t, err)

	for method, want := range map[string]int{
		"Post": 1, "EntryByKey": 1, "EntryByID": 0, "ReversalOf": 0, "Insert": 1, "Account": 1, "Entries": 1, "Unknown": 0,
	} {
		assert.Equal(t, want, store.CallCount(method), method)
	}
}

// Ручки правятся на ходу: тест потребителя включает сбой, пока ручка его
// HTTP-сервера в другой горутине проводит движения. Под -race это чисто.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := ledgertest.NewMemStore(book())
	svc := service(t, store)
	account := uuid.New()
	whileServing(
		func() {
			for i := range 300 {
				_, _ = svc.Post(context.Background(), topup(account, "k"+string(rune('a'+i%26))))
				_, _ = svc.Balance(context.Background(), account)
			}
		},
		func(i int) {
			if i%2 == 0 {
				store.SetErr(errDown)
				return
			}
			store.SetErr(nil)
		},
		func(int) { _ = store.CallCount("Post") },
	)
}

func TestClock(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 16, 12, 0, 0, 5, time.FixedZone("UTC+3", 3*60*60))
	clock := ledgertest.NewClock(at)
	assert.Equal(t, at, clock.Now(), "часы отдают момент как поставили: нормализует сервис")
	clock.Advance(time.Minute)
	assert.Equal(t, at.Add(time.Minute), clock.Now())
	clock.Set(at)
	assert.Equal(t, at, clock.Now())
}

func requireUnavailable(t *testing.T, err, cause error, what string) {
	t.Helper()
	require.ErrorIs(t, err, ledger.ErrUnavailable, what)
	require.ErrorIs(t, err, cause, what)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), what)
}

// whileServing крутит каждую ручку в своей горутине, пока serve не отработает:
// ручка, пишущая мимо замка, не делит с serve ни одной точки синхронизации, и
// -race видит гонку при любом порядке.
func whileServing(serve func(), knobs ...func(i int)) {
	served := make(chan struct{})
	go func() {
		defer close(served)
		serve()
	}()
	var wg sync.WaitGroup
	for _, knob := range knobs {
		wg.Go(func() {
			for i := 0; ; i++ {
				knob(i)
				select {
				case <-served:
					return
				default:
				}
			}
		})
	}
	wg.Wait()
}
