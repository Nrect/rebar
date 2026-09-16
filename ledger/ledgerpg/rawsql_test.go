package ledgerpg_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// Условие выпуска 2 (ADR-0009): «приложение соврало про остаток» — запись идёт
// СЫРЫМ SQL мимо пакета и отбивается базой. Роль — владелец схемы, которому
// привилегии не помеха: держат триггеры, и отказ узнаётся по имени и SQLSTATE.
func TestRawSQL_RefusedByDatabase(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	in := mustPost(t, svc, topup(account, 1000, "raw-in"))

	gap := nextEntry(t, store, account, kindTopup, 10, "raw-gap")
	gap.Seq++
	below := nextEntry(t, store, account, kindSpend, -1001, "raw-below")
	lied := nextEntry(t, store, account, kindTopup, 10, "raw-lied")
	lied.BalanceAfterMinor = 1_000_000

	cases := []struct {
		name       string
		exec       func(ctx context.Context, tx pgx.Tx) error
		code       string
		constraint string
	}{
		{"правка суммы движения", execSQL(`UPDATE ledger_entries SET amount_minor = 5000 WHERE id = $1`, in.ID),
			"23514", "ledger_entries_immutable"},
		{"удаление движения", execSQL(`DELETE FROM ledger_entries WHERE id = $1`, in.ID),
			"23514", "ledger_entries_immutable"},
		{"TRUNCATE журнала", execSQL(`TRUNCATE ledger_entries`), "23514", "ledger_entries_immutable"},
		{"правка остатка", execSQL(`UPDATE ledger_accounts SET balance_minor = 1000000 WHERE account = $1`, account),
			"23514", "ledger_accounts_guard"},
		{"голова на запись, которой нет в журнале",
			execSQL(`UPDATE ledger_accounts SET seq = seq + 1, balance_minor = 1000000, last_hash = $2 WHERE account = $1`,
				account, randomHash(t)),
			"23514", "ledger_accounts_guard"},
		{"удаление счёта", execSQL(`DELETE FROM ledger_accounts WHERE account = $1`, account),
			"23514", "ledger_accounts_guard"},
		{"TRUNCATE счетов вместе с журналом", execSQL(`TRUNCATE ledger_accounts CASCADE`),
			"23514", "ledger_accounts_guard"},
		{"новый счёт с остатком без записей",
			execSQL(`INSERT INTO ledger_accounts (book, account, seq, balance_minor, last_hash) VALUES ($1, $2, 1, 500, $3)`,
				bookName, uuid.New(), randomHash(t)),
			"23514", "ledger_accounts_guard"},
		{"вставка с дырой в номере", insertSQL(gap), "40001", "ledger_entries_chain"},
		// Второй рубеж номера: голова сдвигается ровно на шаг и при снятой проверке.
		// DDL в транзакции случая откатывается вместе с ней.
		{"дыра в номере мимо снятой проверки", func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_check_trg`); err != nil {
				return err
			}
			return insertRaw(ctx, tx, gap)
		}, "40001", "ledger_entries_head_moved"},
		{"остаток после не сходится с суммой", insertSQL(lied), "40001", "ledger_entries_chain"},
		{"запись ниже границы книги", insertSQL(below), "23514", "ledger_entries_floor"},
	}
	for _, tc := range cases {
		tx := beginTx(t, pool)
		requireRefused(t, tc.exec(t.Context(), tx), tc.code, tc.constraint, tc.name)
		require.NoError(t, tx.Rollback(t.Context()))
	}

	head, err := store.Account(t.Context(), bookName, account)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), head.BalanceMinor, "остаток не изменился")
	requireChain(t, svc, account, 1)
}

func execSQL(query string, args ...any) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, args...)
		return err
	}
}

func insertSQL(e ledger.Entry) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error { return insertRaw(ctx, tx, e) }
}

// Разрыв номера и цепи — 40001, ошибки ввода — 23514 и 23503 (ADR-0009,
// решение 2). Разделение классов проверяется по SQLSTATE, а не по тексту, и на
// выходе адаптера: код доезжает очищенным, отказ — своей sentinel.
func TestRefusals_SeparatedBySQLSTATE(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	in := mustPost(t, svc, topup(account, 1000, "class-in"))
	rev, err := svc.Reverse(t.Context(), ledger.ReverseRequest{
		Account: account, EntryID: in.ID, By: bySupport, Reason: testReason, Actor: testActor, IdempotencyKey: "class-rev",
	})
	require.NoError(t, err)
	mustPost(t, svc, topup(account, 5000, "class-cushion"))

	cases := []struct {
		name       string
		spoil      func(e *ledger.Entry)
		want       error
		code       string
		constraint string
		retryable  bool
	}{
		{"дыра в номере", func(e *ledger.Entry) { e.Seq += 2 }, ledger.ErrUnavailable, "40001", "ledger_entries_chain", true},
		{"prev_hash не голова", func(e *ledger.Entry) { e.PrevHash = randomHash(t) }, ledger.ErrUnavailable,
			"40001", "ledger_entries_chain", true},
		{"ключ занят", func(e *ledger.Entry) { e.IdempotencyKey = "class-in" }, ledger.ErrUnavailable,
			"40001", "ledger_entries_taken", true},
		{"знак не по роду", func(e *ledger.Entry) { e.Kind = kindSpend }, ledger.ErrInvalidRequest,
			"23514", "ledger_entries_sign", false},
		{"нет обязательного основания", func(e *ledger.Entry) { e.Reference = "" }, ledger.ErrInvalidRequest,
			"23514", "ledger_entries_required", false},
		{"ниже границы", func(e *ledger.Entry) { e.Kind, e.AmountMinor = kindSpend, -6001 },
			ledger.ErrInsufficientFunds, "23514", "ledger_entries_floor", false},
		{"рода нет в справочнике", func(e *ledger.Entry) { e.Kind = "unknown_kind" }, ledger.ErrUnknownKind,
			"23503", "ledger_entries_kind_fkey", false},
		{"гасимой записи нет на счёте", reversalOf(uuid.New(), -10), ledger.ErrEntryNotFound,
			"23503", "ledger_entries_reversal_target", false},
		{"отмена отмены", reversalOf(rev.ID, 1000), ledger.ErrNotReversible,
			"23514", "ledger_entries_reversal_of_reversal", false},
		{"вторая отмена", reversalOf(in.ID, -1000), ledger.ErrAlreadyReversed,
			"23505", "ux_ledger_entries_reversal", false},
	}
	for _, tc := range cases {
		e := nextEntry(t, store, account, kindTopup, 10, "class-raw")
		tc.spoil(&e)
		e.BalanceAfterMinor = 5000 + e.AmountMinor // портится ровно один инвариант
		err := insertVia(t, store, e)
		require.ErrorIs(t, err, tc.want, tc.name)

		var clean *postgres.Error
		require.ErrorAs(t, err, &clean, tc.name)
		assert.Equal(t, tc.code, clean.Code, "%s: SQLSTATE", tc.name)
		assert.Equal(t, tc.constraint, clean.Constraint, "%s: ограничение", tc.name)
		assert.Equal(t, tc.retryable, postgres.IsRetryable(err), "%s: класс повтора", tc.name)
	}
	requireChain(t, svc, account, 3)
}

// reversalOf — запись становится отменой target на сумму amount.
func reversalOf(target uuid.UUID, amount int64) func(e *ledger.Entry) {
	return func(e *ledger.Entry) {
		e.Kind, e.ReversesID, e.AmountMinor = ledger.KindReversal, &target, amount
	}
}

// 40001 повторяет транзакция потребителя сама: запись по устаревшей голове
// проигрывает гонку, postgres.Runner.InTxRetry пробует снова со свежей головой,
// и вторая попытка проходит. В fn нет require: FailNow внутри раннера оставил бы
// его транзакцию открытой.
func TestRawSQL_ChainRaceIsRetriedByRunner(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 1000, "retry-in"))
	template := nextEntry(t, store, account, kindTopup, 10, "retry-raw")

	runner := postgres.New(pool, postgres.Config{
		LockTimeout: 5 * time.Second, StatementTimeout: 10 * time.Second, MaxAttempts: 3, RetryBase: time.Millisecond,
	})
	var attempts atomic.Int32
	err := runner.InTxRetry(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		head, err := store.Account(ctx, bookName, account)
		if err != nil {
			return err
		}
		if attempts.Add(1) == 1 {
			// Соперник успел между чтением головы и вставкой.
			if _, err = svc.Post(ctx, topup(account, 1, "retry-rival")); err != nil {
				return err
			}
		}
		e := template
		e.Seq, e.PrevHash, e.BalanceAfterMinor = head.Seq+1, head.LastHash, head.BalanceMinor+e.AmountMinor
		return insertRaw(ctx, tx, e)
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), attempts.Load(), "первая попытка проиграла гонку, вторая прошла")

	head, err := store.Account(t.Context(), bookName, account)
	require.NoError(t, err)
	assert.Equal(t, ledger.Account{Seq: 3, BalanceMinor: 1011, LastHash: head.LastHash}, head)
	v, err := svc.Verify(t.Context(), account, ledger.Position{}, 10)
	require.NoError(t, err)
	assert.Equal(t, []ledger.Mismatch{{EntryID: template.ID, Seq: 3, Check: ledger.CheckSignature}}, v.Mismatches,
		"номер, цепь и остаток сошлись; подпись сырой записи база не проверяет — это работа сверки")
}
