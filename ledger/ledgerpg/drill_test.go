package ledgerpg_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgerpg"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// drillCase — правка одного счёта в окне восстановления и то, чем её обязана
// назвать сверка. Цепь счёта: +1000, −300, −200.
type drillCase struct {
	name    string
	account uuid.UUID
	entries []ledger.Entry
	tamper  func(ctx context.Context, tx pgx.Tx, c *drillCase) error
	want    func(c *drillCase) []ledger.Mismatch
	// forged — запись, дописанная в окне; её знает только случай, который её дописал.
	forged ledger.Entry
}

// Условие выпуска 8 (ADR-0009): дамп → восстановление с отключёнными триггерами
// → сверка.
//
// ВОССТАНОВЛЕНИЕ — КАК У pg_restore --disable-triggers: ALTER TABLE … DISABLE
// TRIGGER ALL, запись данных, ENABLE TRIGGER ALL. ALL снимает и системные
// триггеры внешних ключей, поэтому и pg_restore, и тест идут суперпользователем.
// В этом окне остаток не пересчитывается, append-only не держит, и правка
// данных ложится молча.
//
// pg_restore возвращает триггеры режимом ENABLE, а не ALWAYS (замерено на
// pg_dump и pg_restore 16): это видит CheckSchema. ENABLE ALWAYS обратно
// ставит повторный накат миграции — после него схема годна, и правку данных
// видит уже только сверка.
func TestDrill_RestoreWithTriggersDisabled(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	requireSuperuser(t, pool)
	svc := service(t, store)
	cases := drillCases(service(t, store), store)
	for i := range cases {
		cases[i].account = uuid.New()
		cases[i].entries = []ledger.Entry{
			mustPost(t, svc, topup(cases[i].account, 1000, cases[i].name+" in")),
			mustPost(t, svc, spend(cases[i].account, 300, cases[i].name+" out 1")),
			mustPost(t, svc, spend(cases[i].account, 200, cases[i].name+" out 2")),
		}
	}

	tx := beginTx(t, pool)
	for _, table := range []string{"ledger_accounts", "ledger_entries"} {
		_, err := tx.Exec(t.Context(), "ALTER TABLE "+table+" DISABLE TRIGGER ALL")
		require.NoError(t, err)
	}
	for i := range cases {
		if cases[i].tamper != nil {
			require.NoError(t, cases[i].tamper(t.Context(), tx, &cases[i]), cases[i].name)
		}
	}
	for _, table := range []string{"ledger_accounts", "ledger_entries"} {
		_, err := tx.Exec(t.Context(), "ALTER TABLE "+table+" ENABLE TRIGGER ALL")
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(t.Context()))

	require.ErrorContains(t, store.CheckSchema(t.Context()), "не ENABLE ALWAYS", "pg_restore возвращает триггеры режимом ENABLE")
	applyUp(t, pool)
	require.NoError(t, store.CheckSchema(t.Context()), "после повторного наката схема годна: правку видит только сверка")

	found := reconcileBook(t, svc, len(cases))
	for i := range cases {
		c := &cases[i]
		assert.Equal(t, c.want(c), found[c.account], c.name)
	}
}

// drillCases — правки восстановления. Первые четыре названы условием выпуска,
// две следующие — голова, отставшая от журнала; последний счёт не тронут.
func drillCases(forger *ledger.Service, store *ledgerpg.Store) []drillCase {
	return []drillCase{
		{
			name:   "правка суммы записи",
			tamper: drillExec(`UPDATE ledger_entries SET amount_minor = -30 WHERE id = $1`, func(c *drillCase) []any { return []any{c.entries[1].ID} }),
			want: func(c *drillCase) []ledger.Mismatch {
				return []ledger.Mismatch{entryMismatch(c.entries[1], ledger.CheckBalance), entryMismatch(c.entries[1], ledger.CheckSignature)}
			},
		},
		{
			name: "правка остатка счёта",
			tamper: drillExec(`UPDATE ledger_accounts SET balance_minor = balance_minor + 100000 WHERE book = $1 AND account = $2`,
				func(c *drillCase) []any { return []any{bookName, c.account} }),
			want: func(*drillCase) []ledger.Mismatch { return []ledger.Mismatch{{Seq: 3, Check: ledger.CheckHeadBalance}} },
		},
		{
			name:   "удаление записи из середины",
			tamper: drillExec(`DELETE FROM ledger_entries WHERE id = $1`, func(c *drillCase) []any { return []any{c.entries[1].ID} }),
			want: func(c *drillCase) []ledger.Mismatch {
				return []ledger.Mismatch{
					entryMismatch(c.entries[2], ledger.CheckSeq), entryMismatch(c.entries[2], ledger.CheckChain),
					entryMismatch(c.entries[2], ledger.CheckBalance),
				}
			},
		},
		{
			// Номер, цепь и остаток сходятся, голова переписана на запись: выдаёт её
			// только подпись.
			name: "дописанная запись с чужой подписью",
			tamper: func(ctx context.Context, tx pgx.Tx, c *drillCase) error {
				forged, err := forger.WithStore(store.WithTx(tx)).Post(ctx, topup(c.account, 500, c.name))
				if err != nil {
					return err
				}
				c.forged = forged
				_, err = tx.Exec(ctx, `UPDATE ledger_accounts SET seq = $3, balance_minor = $4, last_hash = $5
					WHERE book = $1 AND account = $2`, bookName, c.account, forged.Seq, forged.BalanceAfterMinor, forged.EntryHash)
				return err
			},
			want: func(c *drillCase) []ledger.Mismatch {
				return []ledger.Mismatch{entryMismatch(c.forged, ledger.CheckSignature)}
			},
		},
		{
			name: "потерянная строка счёта",
			tamper: drillExec(`DELETE FROM ledger_accounts WHERE book = $1 AND account = $2`,
				func(c *drillCase) []any { return []any{bookName, c.account} }),
			want: func(*drillCase) []ledger.Mismatch { return []ledger.Mismatch{{Seq: 0, Check: ledger.CheckHeadChain}} },
		},
		{
			name: "голова откачена к ранней записи",
			tamper: drillExec(`UPDATE ledger_accounts SET seq = 2, balance_minor = $3, last_hash = $4 WHERE book = $1 AND account = $2`,
				func(c *drillCase) []any {
					return []any{bookName, c.account, c.entries[1].BalanceAfterMinor, c.entries[1].EntryHash}
				}),
			want: func(*drillCase) []ledger.Mismatch { return []ledger.Mismatch{{Seq: 2, Check: ledger.CheckHeadChain}} },
		},
		{
			name: "нетронутый счёт",
			want: func(*drillCase) []ledger.Mismatch { return nil },
		},
	}
}

// drillExec — правка одним запросом с аргументами случая.
func drillExec(query string, args func(c *drillCase) []any) func(ctx context.Context, tx pgx.Tx, c *drillCase) error {
	return func(ctx context.Context, tx pgx.Tx, c *drillCase) error {
		_, err := tx.Exec(ctx, query, args(c)...)
		return err
	}
}

func entryMismatch(e ledger.Entry, check ledger.Check) ledger.Mismatch {
	return ledger.Mismatch{EntryID: e.ID, Seq: e.Seq, Check: check}
}

// reconcileBook — круг сверки по книге порциями меньше счетов; находки по
// счетам. Найденное дважды — дефект обхода.
func reconcileBook(t *testing.T, svc *ledger.Service, accounts int) map[uuid.UUID][]ledger.Mismatch {
	t.Helper()
	obs := ledgertest.NewObserver()
	const batch = 4
	rec := ledger.NewReconciler(svc, obs, ledger.ReconcileConfig{Accounts: batch, Page: 2})
	for range accounts/batch + 1 {
		_, err := rec.Run(t.Context())
		require.NoError(t, err)
	}
	found := map[uuid.UUID][]ledger.Mismatch{}
	for _, f := range obs.Findings() {
		require.Equal(t, bookName, f.Book)
		require.NotContains(t, found, f.Account, "счёт найден дважды за круг")
		found[f.Account] = f.Mismatches
	}
	return found
}

// requireSuperuser — учения идут суперпользователем, как pg_restore
// --disable-triggers: снять системные триггеры внешних ключей владельцу таблицы
// не позволено.
func requireSuperuser(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	require.True(t, super, "учения восстановления требуют суперпользователя, как pg_restore --disable-triggers")
}
