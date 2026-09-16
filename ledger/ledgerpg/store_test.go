package ledgerpg_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgerpg"
	"github.com/nrect/rebar/postgres"
)

// lazyPool — пул, который не соединяется, пока его не позовут: конструктору
// нужен настоящий *pgxpool.Pool, а базе здесь делать нечего.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://ledgerpg@127.0.0.1:1/none")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// Fail closed: негодная сборка приложения падает на старте, а не на первом
// движении. Тексты — как у ledgertest.NewMemStore: тот же конструктор.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	pool := lazyPool(t)
	assert.PanicsWithValue(t, "ledgerpg.New: nil pool", func() { ledgerpg.New(nil, wallet()) })
	assert.PanicsWithValue(t, "ledgerpg.New: at least one book must be registered", func() { ledgerpg.New(pool) })
	bad := wallet()
	bad.Unit = ""
	assert.PanicsWithValue(t, "ledgerpg.New: Book.Unit must match [A-Za-z0-9_]{1,16}", func() { ledgerpg.New(pool, bad) })
	assert.PanicsWithValue(t, `ledgerpg.New: book "wallet" is registered twice`, func() { ledgerpg.New(pool, wallet(), wallet()) })
	assert.PanicsWithValue(t, "ledgerpg.WithTx: nil tx", func() { ledgerpg.New(pool, wallet()).WithTx(nil) })
}

// То, что адаптер решает до запроса, отмена не перебивает (как у двойника):
// потолок выборки и незаведённая книга. Чужой счёт — под блокировкой, ниже.
func TestStore_ArgumentsBeforeQuery(t *testing.T) {
	t.Parallel()

	store := ledgerpg.New(lazyPool(t), wallet())
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := store.Entries(cancelled, bookName, uuid.New(), 0, 0)
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	require.NotErrorIs(t, err, context.Canceled)

	called := false
	err = store.Post(t.Context(), "points", uuid.New(), func(ledger.AccountTx, ledger.Account) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ledger.ErrInvalidRequest, "книга не заведена в New")
	assert.False(t, called)
	assert.Equal(t, errs.KindUnknown, errs.KindOf(err), "книги нет — дефект сборки, а не сбой")

	err = store.Post(cancelled, bookName, uuid.New(), func(ledger.AccountTx, ledger.Account) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	assert.False(t, called, "fn по отменённому контексту не зовётся")
}

// Книга заведена в New, но её нет в справочнике базы: движение отбивает внешний
// ключ счёта, и это тот же отказ, что у незаведённой книги. Чтение неизвестной
// книги — пустая выборка, как у двойника.
func TestStore_BookMissingInDirectory(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	applyUp(t, pool)
	store := ledgerpg.New(pool, wallet()) // справочник миграцией потребителя не заполнен
	svc := service(t, store)
	account := uuid.New()
	_, err := svc.Post(t.Context(), topup(account, 100, "no-book"))
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	assert.Equal(t, errs.KindUnknown, errs.KindOf(err))
	assert.Contains(t, err.Error(), "ledger_accounts_book_fkey")

	head, err := store.Account(t.Context(), "points", account)
	require.NoError(t, err)
	assert.Equal(t, ledger.Account{}, head)
}

// Счёт под блокировкой отказывает так же, как у двойника: отменённый посреди fn
// контекст — ErrUnavailable с причиной в цепочке, у каждого метода и у фиксации.
func TestStore_AccountTxFailsLikeDouble(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, wallet())
	account := uuid.New()
	ctx, cancel := context.WithCancel(t.Context())
	err := store.Post(ctx, bookName, account, func(tx ledger.AccountTx, _ ledger.Account) error {
		cancel()
		// require внутри fn безопасен: FailNow уходит через Goexit, и откат Post
		// в defer отрабатывает.
		_, _, err := tx.EntryByKey(ctx, "k")
		require.ErrorIs(t, err, ledger.ErrUnavailable)
		require.ErrorIs(t, err, context.Canceled, "EntryByKey")
		_, _, err = tx.EntryByID(ctx, uuid.New())
		require.ErrorIs(t, err, context.Canceled, "EntryByID")
		_, _, err = tx.ReversalOf(ctx, uuid.New())
		require.ErrorIs(t, err, context.Canceled, "ReversalOf")
		err = tx.Insert(ctx, ledger.Entry{Book: bookName, Account: account})
		require.ErrorIs(t, err, context.Canceled, "Insert")
		err = tx.Insert(ctx, ledger.Entry{Book: bookName, Account: uuid.New()})
		require.ErrorIs(t, err, ledger.ErrInvalidRequest, "чужой счёт виден до запроса")
		require.NotErrorIs(t, err, context.Canceled)
		return nil
	})
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	require.ErrorIs(t, err, context.Canceled, "фиксация")
}

// Счёт — пара книги и счёта: у кошелька и баллов покупателя один uuid. Выборки
// адаптера и проверки триггера сужены обоими ключами — иначе повтор по ключу
// взял бы запись другой книги, отмена погасила бы чужое, а голову счёта подпёрла
// бы чужая запись.
func TestStore_ScopesByBookAndAccount(t *testing.T) {
	t.Parallel()

	// У баллов граница ниже, и их справочник заведён первым: выборка рода без
	// книги взяла бы их строку.
	points := wallet()
	points.Name, points.Unit, points.Floor = "points", "points", -1000
	store, pool := newStore(t, points, wallet())
	walletSvc, pointsSvc := service(t, store), serviceFor(t, store, points)
	customer, stranger := uuid.New(), uuid.New()

	w1 := mustPost(t, walletSvc, topup(customer, 1000, "shared"))
	p1 := mustPost(t, pointsSvc, topup(customer, 50, "shared"))
	assert.NotEqual(t, w1.ID, p1.ID, "тот же ключ в другой книге — другое движение")
	assert.Equal(t, w1.ID, mustPost(t, walletSvc, topup(customer, 1000, "shared")).ID, "повтор отдаёт запись своей книги")
	s1 := mustPost(t, walletSvc, topup(stranger, 1000, "shared"))
	// Кошелёк уходит на номер 2, пока баллы на 1: запись головы сужена книгой.
	mustPost(t, walletSvc, topup(customer, 1, "second"))
	mustPost(t, pointsSvc, topup(customer, 5, "second"))
	p3 := mustPost(t, pointsSvc, topup(customer, 7, "third"))

	for _, tc := range []struct {
		svc     *ledger.Service
		account uuid.UUID
		balance int64
		entries int
	}{
		{walletSvc, customer, 1001, 2}, {pointsSvc, customer, 62, 3}, {walletSvc, stranger, 1000, 1},
	} {
		balance, err := tc.svc.Balance(t.Context(), tc.account)
		require.NoError(t, err)
		assert.Equal(t, tc.balance, balance)
		requireChain(t, tc.svc, tc.account, tc.entries)
	}

	// Чужую запись ядро не находит у себя и до базы не доходит.
	for _, tc := range []struct {
		name  string
		entry uuid.UUID
	}{{"запись другой книги", p1.ID}, {"запись другого счёта", s1.ID}} {
		_, err := walletSvc.Reverse(t.Context(), ledger.ReverseRequest{
			Account: customer, EntryID: tc.entry, By: bySupport, Reason: testReason, Actor: testActor,
			IdempotencyKey: "reverse " + tc.name,
		})
		require.ErrorIs(t, err, ledger.ErrEntryNotFound, tc.name)
		var clean *postgres.Error
		assert.NotErrorAs(t, err, &clean, "%s: отказ ядра до запроса, а не базы", tc.name)
	}

	// Мимо ядра книгу держит база.
	foreign := nextEntry(t, store, customer, ledger.KindReversal, -p1.AmountMinor, "raw-other-book")
	foreign.ReversesID = &p1.ID
	tx := beginTx(t, pool)
	requireRefused(t, insertRaw(t.Context(), tx, foreign), "23503", "ledger_entries_reversal_target", "отмена записи другой книги")
	require.NoError(t, tx.Rollback(t.Context()))

	tx = beginTx(t, pool)
	_, err := tx.Exec(t.Context(), `UPDATE ledger_accounts SET seq = seq + 1, balance_minor = $3, last_hash = $4
		WHERE book = $1 AND account = $2`, bookName, customer, p3.BalanceAfterMinor, p3.EntryHash)
	requireRefused(t, err, "23514", "ledger_accounts_guard", "голова кошелька на запись баллов")
	require.NoError(t, tx.Rollback(t.Context()))

	below := nextEntry(t, store, customer, kindSpend, -1002, "raw-below-wallet")
	tx = beginTx(t, pool)
	requireRefused(t, insertRaw(t.Context(), tx, below), "23514", "ledger_entries_floor", "списание кошелька ниже его границы")
	require.NoError(t, tx.Rollback(t.Context()))
	requireChain(t, walletSvc, customer, 2)
}

// mutation — запрос, меняющий строки; блокировка FOR NO KEY UPDATE к ним не
// относится и вырезается до поиска.
var mutation = regexp.MustCompile(`(?i)\b(UPDATE|DELETE|TRUNCATE)\b`)

// Журнал и голова счёта append-only не только по триггеру, но и по коду: в
// строковых литералах адаптера нет ни UPDATE, ни DELETE, ни TRUNCATE — голову
// пишет только триггер. Литералы, а не текст файла: комментарии запрет как раз
// объясняют.
func TestAdapter_HasNoMutations(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	literals := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			literals++
			query := strings.ReplaceAll(value, "FOR NO KEY UPDATE", "")
			assert.False(t, mutation.MatchString(query), "%s:%d: запрос адаптера меняет строки: %q",
				name, fset.Position(lit.Pos()).Line, value)
			return true
		})
	}
	require.NotZero(t, literals, "строковых литералов не нашлось — тест смотрит не туда")
}
