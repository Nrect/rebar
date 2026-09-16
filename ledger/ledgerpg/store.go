package ledgerpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// Store — ledger.Store поверх таблиц ledger_* (Migrations).
type Store struct {
	// pool и tx исключают друг друга: New даёт пул, WithTx — транзакцию
	// потребителя, в которой Post ставит точку сохранения, а не открывает свою.
	pool  *pgxpool.Pool
	tx    pgx.Tx
	books map[string]bookSpec
}

var _ ledger.Store = (*Store)(nil)

// bookSpec — книга так, как её сверяет CheckSchema; копия, и правка реестра
// вызывающим её не меняет.
type bookSpec struct {
	unit  string
	floor int64
	kinds []ledger.KindSpec
}

// New паникует на nil-пуле, пустом списке книг, негодной книге и повторе имени:
// ошибка сборки падает на старте. Книги те же, что у ledgertest.NewMemStore, —
// тест и прод отличаются одним конструктором.
func New(pool *pgxpool.Pool, books ...ledger.Book) *Store {
	if pool == nil {
		panic("ledgerpg.New: nil pool")
	}
	if len(books) == 0 {
		panic("ledgerpg.New: at least one book must be registered")
	}
	registered := make(map[string]bookSpec, len(books))
	for _, book := range books {
		if err := book.Validate(); err != nil {
			panic("ledgerpg.New: " + err.Error())
		}
		if _, dup := registered[book.Name]; dup {
			panic(fmt.Sprintf("ledgerpg.New: book %q is registered twice", book.Name))
		}
		registered[book.Name] = bookSpec{unit: book.Unit, floor: book.Floor, kinds: book.AllKinds()}
	}
	return &Store{pool: pool, books: registered}
}

// WithTx — тот же адаптер в транзакции потребителя: движение ложится вместе с
// его бизнес-фактом (решение 11). Отказ движения эту транзакцию не рвёт:
// Post откатывает свою точку сохранения (уточнение арбитра 6).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("ledgerpg.WithTx: nil tx")
	}
	return &Store{tx: tx, books: s.books}
}

// db — исполнитель чтения: транзакция потребителя, если она есть.
func (s *Store) db() postgres.Querier {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// Запросы счёта. Счёт заводится при первом движении; уже заведённый ON CONFLICT
// не трогает и транзакцию не роняет.
const (
	openAccountSQL = `INSERT INTO ledger_accounts (book, account) VALUES ($1, $2)
ON CONFLICT (book, account) DO NOTHING`
	// FOR NO KEY UPDATE (решение 9): внешние ключи записей и читатели остатка
	// блокировку не ждут.
	lockAccountSQL = `SELECT seq, balance_minor, last_hash FROM ledger_accounts
WHERE book = $1 AND account = $2 FOR NO KEY UPDATE`
	accountSQL = `SELECT seq, balance_minor, last_hash FROM ledger_accounts
WHERE book = $1 AND account = $2`
	entriesSQL = `SELECT ` + entryColumns + ` FROM ledger_entries
WHERE book = $1 AND account = $2 AND seq > $3 ORDER BY seq LIMIT $4`
)

// Post — блокировка строки счёта и fn под ней (контракт ledger.Store.Post).
// Ошибку fn отдаёт тем же значением и не оставляет ни одной её вставки.
func (s *Store) Post(ctx context.Context, book string, account uuid.UUID,
	fn func(tx ledger.AccountTx, head ledger.Account) error,
) error {
	if ctx.Err() != nil {
		return storeError("post", ctx.Err())
	}
	if _, ok := s.books[book]; !ok {
		return fmt.Errorf("%w: %q", errUnknownBook, book)
	}
	u, err := s.begin(ctx)
	if err != nil {
		return storeError("post: begin", err)
	}
	// Откат и на панике fn: иначе точка сохранения с её вставками осталась бы в
	// транзакции потребителя.
	committed := false
	defer func() {
		if !committed {
			u.abort(ctx)
		}
	}()
	if fnErr := locked(ctx, u.tx, book, account, fn); fnErr != nil {
		return fnErr
	}
	if commitErr := u.commit(ctx); commitErr != nil {
		return storeError("post: commit", commitErr)
	}
	committed = true
	return nil
}

// locked — счёт заводится и блокируется, fn получает голову на момент
// блокировки. Вне fn счёт не живёт.
func locked(ctx context.Context, tx pgx.Tx, book string, account uuid.UUID,
	fn func(tx ledger.AccountTx, head ledger.Account) error,
) error {
	if _, err := tx.Exec(ctx, openAccountSQL, book, account); err != nil {
		return refusal("post: open account", err)
	}
	var head ledger.Account
	if err := tx.QueryRow(ctx, lockAccountSQL, book, account).Scan(&head.Seq, &head.BalanceMinor, &head.LastHash); err != nil {
		return storeError("post: lock account", err)
	}
	acct := &accountTx{tx: tx, book: book, account: account}
	defer acct.done.Store(true)
	return fn(acct, head)
}

// Account — голова счёта без блокировки; у счёта без движений — нулевая.
func (s *Store) Account(ctx context.Context, book string, account uuid.UUID) (ledger.Account, error) {
	var head ledger.Account
	err := s.db().QueryRow(ctx, accountSQL, book, account).Scan(&head.Seq, &head.BalanceMinor, &head.LastHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ledger.Account{}, nil
	case err != nil:
		return ledger.Account{}, storeError("account", err)
	}
	return head, nil
}

// Entries — записи счёта после afterSeq по возрастанию номера, не больше limit.
// Потолок проверяется до запроса: отмена контекста его не перебивает.
func (s *Store) Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int,
) ([]ledger.Entry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: ledgerpg: limit must be positive, got %d", ledger.ErrInvalidRequest, limit)
	}
	rows, err := s.db().Query(ctx, entriesSQL, book, account, afterSeq, limit)
	if err != nil {
		return nil, storeError("entries", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ledger.Entry, error) {
		return scanEntry(row)
	})
	if err != nil {
		return nil, storeError("entries", err)
	}
	return entries, nil
}

// Обход сверки — два чтения по индексам (ключ счёта и ux_ledger_entries_seq),
// каждое до limit, и слияние в Go. uuid Postgres сравнивает побайтно — тот же
// порядок, что у bytes.Compare.
const (
	accountsSQL = `SELECT account FROM ledger_accounts
WHERE book = $1 AND account > $2 ORDER BY account LIMIT $3`
	entryAccountsSQL = `SELECT DISTINCT account FROM ledger_entries
WHERE book = $1 AND account > $2 ORDER BY account LIMIT $3`
)

// Accounts — счета книги после after по возрастанию, не больше limit
// (контракт ledger.Store.Accounts).
//
// СЧЕТА И ИЗ ЖУРНАЛА ТОЖЕ: восстановление, потерявшее строки ledger_accounts,
// оставляет записи без головы — остаток таких счетов читается нулём. Обход по
// одной таблице счетов их не видел бы, и сверка молчала бы ровно там, где
// деньги пропали.
func (s *Store) Accounts(ctx context.Context, book string, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: ledgerpg: limit must be positive, got %d", ledger.ErrInvalidRequest, limit)
	}
	opened, err := s.accountIDs(ctx, accountsSQL, book, after, limit)
	if err != nil {
		return nil, err
	}
	posted, err := s.accountIDs(ctx, entryAccountsSQL, book, after, limit)
	if err != nil {
		return nil, err
	}
	// Первые limit объединения лежат среди первых limit каждой выборки.
	ids := slices.Concat(opened, posted)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	return ids[:min(limit, len(ids))], nil
}

func (s *Store) accountIDs(ctx context.Context, query, book string, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.db().Query(ctx, query, book, after, limit)
	if err != nil {
		return nil, storeError("accounts", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, storeError("accounts", err)
	}
	return ids, nil
}

// Точка сохранения своя, SQL-ом, а не вложенной pgx.Tx: та закрывается и на
// неудачном RELEASE, и откатить его было бы уже нечем — вставки fn остались бы
// в транзакции потребителя и ушли бы в базу с её фиксацией.
const (
	savepointSQL = `SAVEPOINT ledgerpg_post`
	releaseSQL   = `RELEASE SAVEPOINT ledgerpg_post`
	// Откат и снятие одним запросом: точки сохранения не копятся в транзакции
	// потребителя от движения к движению.
	rollbackToSQL = `ROLLBACK TO SAVEPOINT ledgerpg_post; RELEASE SAVEPOINT ledgerpg_post`
)

// unit — транзакция одного Post: своя из пула либо точка сохранения в
// транзакции потребителя.
type unit struct {
	tx        pgx.Tx
	savepoint bool
}

func (s *Store) begin(ctx context.Context) (unit, error) {
	if s.tx == nil {
		tx, err := s.pool.Begin(ctx)
		return unit{tx: tx}, err
	}
	if _, err := s.tx.Exec(ctx, savepointSQL); err != nil {
		return unit{}, err
	}
	return unit{tx: s.tx, savepoint: true}, nil
}

func (u unit) commit(ctx context.Context) error {
	if !u.savepoint {
		return u.tx.Commit(ctx)
	}
	_, err := u.tx.Exec(ctx, releaseSQL)
	return err
}

// abort — откат мимо отмены ctx: по отменённому ctx pgx не отправил бы ROLLBACK,
// а закрыл бы соединение. Ошибка отката не возвращается — наружу уже уходит та,
// из-за которой откатываемся.
func (u unit) abort(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if !u.savepoint {
		_ = u.tx.Rollback(ctx)
		return
	}
	_, _ = u.tx.Exec(ctx, rollbackToSQL)
}
