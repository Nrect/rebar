package ledgerpg_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
)

// shopOrders — таблица потребителя: его бизнес-факт в той же транзакции.
func shopOrders(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `CREATE TABLE shop_orders (id UUID PRIMARY KEY)`)
	require.NoError(t, err)
}

// WithTx — та же транзакция, что у бизнес-факта (решение 11). После отката из
// базы читаются ОБЕ стороны: заказ, запись, счёт, заведённый первым движением, —
// и ключ идемпотентности свободен для законного повтора.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	shopOrders(t, pool)
	svc := service(t, store)
	account := uuid.New()
	order := uuid.New()

	tx := beginTx(t, pool)
	_, err := tx.Exec(t.Context(), `INSERT INTO shop_orders (id) VALUES ($1)`, order)
	require.NoError(t, err)
	_, err = svc.WithStore(store.WithTx(tx)).Post(t.Context(), topup(account, 1000, "atomic"))
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(t.Context()))

	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM ledger_entries`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM ledger_accounts`))

	tx = beginTx(t, pool)
	_, err = tx.Exec(t.Context(), `INSERT INTO shop_orders (id) VALUES ($1)`, order)
	require.NoError(t, err)
	e, err := svc.WithStore(store.WithTx(tx)).Post(t.Context(), topup(account, 1000, "atomic"))
	require.NoError(t, err, "ключ свободен: законный повтор проходит")
	require.NoError(t, tx.Commit(t.Context()))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
	assert.Equal(t, int64(1), e.Seq)
	requireChain(t, svc, account, 1)
}

// Уточнение арбитра 6: отказ движения на пути WithTx не рвёт транзакцию
// потребителя — адаптер откатывает свою точку сохранения. Отказ приходит от
// ядра, от базы, от базы через fn, проглотившую ошибку, и от самой fn; после
// каждого потребитель делает свой запрос, и фиксация проходит.
func TestStore_WithTx_RefusalKeepsTxUsable(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	shopOrders(t, pool)
	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 100, "usable-in"))

	tx := beginTx(t, pool)
	inTx := store.WithTx(tx)
	order := func(what string) {
		t.Helper()
		_, err := tx.Exec(t.Context(), `INSERT INTO shop_orders (id) VALUES ($1)`, uuid.New())
		require.NoError(t, err, "запрос потребителя после отказа: %s", what)
	}

	// Отказ ядра под блокировкой: запросов с ошибкой не было.
	_, err := svc.WithStore(inTx).Post(t.Context(), spend(account, 500, "usable-over"))
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	order("отказ ядра")

	// Отказ базы: триггер отбил вставку, и транзакция Postgres встала в aborted.
	bad := nextEntry(t, store, account, kindSpend, 50, "usable-sign")
	err = inTx.Post(t.Context(), bookName, account, func(acct ledger.AccountTx, _ ledger.Account) error {
		return acct.Insert(t.Context(), bad)
	})
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	order("отказ базы")

	// Отказ базы, который fn проглотила: фиксация точки сохранения не проходит.
	err = inTx.Post(t.Context(), bookName, account, func(acct ledger.AccountTx, _ ledger.Account) error {
		_ = acct.Insert(t.Context(), bad)
		return nil
	})
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	order("проглоченный отказ базы")

	// Ошибка fn после удачной вставки: вставка уходит вместе с точкой сохранения.
	good := nextEntry(t, store, account, kindSpend, -30, "usable-rolled")
	err = inTx.Post(t.Context(), bookName, account, func(acct ledger.AccountTx, _ ledger.Account) error {
		if insertErr := acct.Insert(t.Context(), good); insertErr != nil {
			return insertErr
		}
		return errPlanned
	})
	require.ErrorIs(t, err, errPlanned, "ошибка fn отдаётся тем же значением")
	order("ошибка fn")

	mustPost(t, svc.WithStore(inTx), spend(account, 40, "usable-spend"))
	require.NoError(t, tx.Commit(t.Context()))

	assert.Equal(t, 4, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
	head, err := store.Account(t.Context(), bookName, account)
	require.NoError(t, err)
	assert.Equal(t, int64(60), head.BalanceMinor, "легло только последнее движение")
	requireChain(t, svc, account, 2)
}

// Счёт, утёкший из fn, в транзакции потребителя мёртв: она ещё открыта, и без
// этого запись шла бы мимо блокировки.
func TestStore_WithTx_LeakedAccountIsDead(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	tx := beginTx(t, pool)
	inTx := store.WithTx(tx)

	var leaked ledger.AccountTx
	require.NoError(t, inTx.Post(t.Context(), bookName, account, func(acct ledger.AccountTx, _ ledger.Account) error {
		leaked = acct
		return nil
	}))
	_, _, err := leaked.EntryByKey(t.Context(), "leaked")
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	_, _, err = leaked.EntryByID(t.Context(), uuid.New())
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	_, _, err = leaked.ReversalOf(t.Context(), uuid.New())
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	require.ErrorIs(t, leaked.Insert(t.Context(), nextEntry(t, store, account, kindTopup, 10, "leaked")), ledger.ErrUnavailable)

	require.NoError(t, tx.Commit(t.Context()))
	requireChain(t, svc, account, 0)
}
