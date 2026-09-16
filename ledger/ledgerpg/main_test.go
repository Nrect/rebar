package ledgerpg_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgerpg"
	"github.com/nrect/rebar/ledger/ledgertest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд общий
// с остальными адаптерами тулкита: он умеет TEST_DATABASE_URL, без которого
// мутационный прогон поднимал бы контейнер на каждого мутанта.
var db *pgtest.DB

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	started, err := pgtest.Start(ctx, pgtest.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "старт Postgres:", err)
		os.Exit(1)
	}
	db = started
	code := m.Run()
	db.Close(ctx)
	os.Exit(code)
}

// Данные тестов: книга кошелька с родами всех знаков и обеих обязательностей.
const (
	bookName    = "wallet"
	kindTopup   = "topup"
	kindSpend   = "spend"
	kindAdjust  = "adjustment"
	bySupport   = "support"
	testActor   = "staff:1"
	testReason  = "manual correction"
	refTopupPfx = "payment:"
)

func wallet() ledger.Book {
	return ledger.Book{
		Name: bookName, Unit: "RUB",
		Kinds: []ledger.KindSpec{
			{
				Name: kindTopup, Sign: ledger.SignCredit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{bySupport},
			},
			{Name: kindSpend, Sign: ledger.SignDebit, Reference: ledger.Required, Attribution: ledger.Optional},
			{Name: kindAdjust, Sign: ledger.SignAny, Reference: ledger.Optional, Attribution: ledger.Required},
		},
	}
}

// newSchemaPool — пул в пустую схему теста: миграции ещё не применены.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// newStore — схема на тест, накат каталога миграций, справочник книг и адаптер
// над ними. Накатывается сам каталог, а не его копия в коде: тестируется
// артефакт, который уедет раннеру потребителя (applyUp — pgtestcopy_test.go).
func newStore(t *testing.T, books ...ledger.Book) (*ledgerpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	applyUp(t, pool)
	for _, book := range books {
		seedBook(t, pool, book)
	}
	return ledgerpg.New(pool, books...), pool
}

// seedBook — справочник так, как его пишет миграция потребителя из Config.
func seedBook(t *testing.T, pool *pgxpool.Pool, book ledger.Book) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `INSERT INTO ledger_books (book, unit, floor_minor) VALUES ($1, $2, $3)`,
		book.Name, book.Unit, book.Floor)
	require.NoError(t, err)
	for _, kind := range book.AllKinds() {
		_, err = pool.Exec(t.Context(),
			`INSERT INTO ledger_kinds (book, kind, sign, reference, attribution) VALUES ($1, $2, $3, $4, $5)`,
			book.Name, kind.Name, string(kind.Sign), string(kind.Reference), string(kind.Attribution))
		require.NoError(t, err)
	}
}

// service — кошелёк поверх хранилища.
func service(t *testing.T, store ledger.Store) *ledger.Service {
	t.Helper()
	return serviceFor(t, store, wallet())
}

// serviceFor — книга поверх хранилища со своим ключом и часами на моменте БД.
func serviceFor(t *testing.T, store ledger.Store, book ledger.Book) *ledger.Service {
	t.Helper()
	key, err := secrets.GenerateKey()
	require.NoError(t, err)
	svc := ledger.NewService(store, ledger.Config{Book: book, Keys: map[secrets.KeyID][]byte{1: key}, ActiveKey: 1})
	svc.SetClock(ledgertest.NewClock(pgtest.Now()).Now)
	return svc
}

func topup(account uuid.UUID, amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: account, Kind: kindTopup, AmountMinor: amount, Reference: refTopupPfx + key, IdempotencyKey: key,
	}
}

func spend(account uuid.UUID, amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: account, Kind: kindSpend, AmountMinor: -amount, Reference: "order:" + key, IdempotencyKey: key,
	}
}

func mustPost(t *testing.T, svc *ledger.Service, req ledger.PostRequest) ledger.Entry {
	t.Helper()
	e, err := svc.Post(t.Context(), req)
	require.NoError(t, err, "движение %s", req.IdempotencyKey)
	return e
}

// nextEntry — запись мимо ядра, годная для схемы: номер, цепь и остаток по
// голове. Подпись — случайные байты: база её не проверяет.
func nextEntry(t *testing.T, store ledger.Store, account uuid.UUID, kind string, amount int64, key string) ledger.Entry {
	t.Helper()
	head, err := store.Account(t.Context(), bookName, account)
	require.NoError(t, err)
	prev := head.LastHash
	if head.Seq == 0 {
		prev = make([]byte, ledger.HashSize)
	}
	return ledger.Entry{
		ID: uuid.New(), Book: bookName, Account: account, Seq: head.Seq + 1,
		Kind: kind, AmountMinor: amount, BalanceAfterMinor: head.BalanceMinor + amount,
		Reference: "raw:" + key, Reason: testReason, Actor: testActor, IdempotencyKey: key,
		CreatedAt: pgtest.Now(), KeyID: 1, PrevHash: bytes.Clone(prev), EntryHash: randomHash(t),
	}
}

func randomHash(t *testing.T) []byte {
	t.Helper()
	hash, err := secrets.GenerateKey() // 32 случайных байта — длина подписи
	require.NoError(t, err)
	return hash
}

// insertRaw — та же запись сырым SQL мимо адаптера.
func insertRaw(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}, e ledger.Entry,
) error {
	_, err := q.Exec(ctx, `INSERT INTO ledger_entries (id, book, account, seq, kind, amount_minor,
		balance_after_minor, reference, reverses_id, reason, actor, idempotency_key, created_at, key_id,
		prev_hash, entry_hash) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		e.ID, e.Book, e.Account, e.Seq, e.Kind, e.AmountMinor, e.BalanceAfterMinor, e.Reference, e.ReversesID,
		e.Reason, e.Actor, e.IdempotencyKey, e.CreatedAt, int32(e.KeyID), e.PrevHash, e.EntryHash)
	return err
}

// insertVia — запись через адаптер в одной транзакции Post.
func insertVia(t *testing.T, store ledger.Store, e ledger.Entry) error {
	t.Helper()
	return store.Post(t.Context(), e.Book, e.Account, func(tx ledger.AccountTx, _ ledger.Account) error {
		return tx.Insert(t.Context(), e)
	})
}

// requireRefused — база отказала именно этим: SQLSTATE и именем ограничения.
func requireRefused(t *testing.T, err error, code, constraint, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, what)
	assert.Equal(t, code, pgErr.Code, "%s: SQLSTATE", what)
	assert.Equal(t, constraint, pgErr.ConstraintName, "%s: ограничение", what)
}

// beginTx — транзакция потребителя. Откат сразу в Cleanup: тест, упавший до
// своего Rollback, иначе вешал бы pool.Close на занятом соединении до таймаута.
func beginTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query).Scan(&n))
	return n
}

// requireChain — записи счёта, их ровно n, и цепь сходится вместе с подписями.
func requireChain(t *testing.T, svc *ledger.Service, account uuid.UUID, n int) {
	t.Helper()
	v, err := svc.Verify(t.Context(), account, ledger.Position{}, 1000)
	require.NoError(t, err)
	assert.Equal(t, n, v.Checked, "записей на счёте")
	assert.Empty(t, v.Mismatches, "цепь сходится")
}

// errPlanned — ошибка fn, которую тест ждёт обратно тем же значением.
var errPlanned = errors.New("ledgerpg_test: fn refused")
