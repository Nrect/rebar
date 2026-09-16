package ledgerpg_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
)

// appGrants — права роли приложения ровно так, как их даёт doc.go: рецепт,
// разошедшийся с тестом, проверял бы не то, что прочтёт потребитель.
var appGrants = []string{
	"GRANT SELECT ON ledger_books, ledger_kinds, ledger_accounts, ledger_entries TO app;",
	"GRANT INSERT (book, account), UPDATE (version) ON ledger_accounts TO app;",
	"GRANT INSERT ON ledger_entries TO app;",
}

func TestPrivileges_RecipeIsInDoc(t *testing.T) {
	t.Parallel()

	doc, err := os.ReadFile("doc.go")
	require.NoError(t, err)
	for _, grant := range appGrants {
		assert.Contains(t, string(doc), "//\t"+grant, "рецепт прав в doc.go")
	}
}

// Условие выпуска 3 (ADR-0009): роль приложения не может UPDATE, DELETE и
// TRUNCATE журнала и не может править остаток, но может вставлять и брать
// блокировку. Отказ — 42501 до триггеров: это третий, независимый уровень.
func TestPrivileges_AppRole(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	role := appRole(t, pool)
	svc := service(t, store)
	account := uuid.New()
	in := mustPost(t, svc, topup(account, 1000, "priv-in"))

	// Может: движение целиком — счёт, блокировка, запись; остаток пишет триггер.
	tx := beginAs(t, pool, role)
	_, err := svc.WithStore(store.WithTx(tx)).Post(t.Context(), spend(account, 300, "priv-spend"))
	require.NoError(t, err, "движение под ролью приложения")
	_, err = svc.WithStore(store.WithTx(tx)).Post(t.Context(), topup(uuid.New(), 50, "priv-new"))
	require.NoError(t, err, "первое движение нового счёта под ролью приложения")
	require.NoError(t, tx.Commit(t.Context()))

	tx = beginAs(t, pool, role)
	var seq int64
	require.NoError(t, tx.QueryRow(t.Context(),
		`SELECT seq FROM ledger_accounts WHERE book = $1 AND account = $2 FOR NO KEY UPDATE`, bookName, account).Scan(&seq),
		"блокировка строки счёта")
	assert.Equal(t, int64(2), seq)
	require.NoError(t, tx.Rollback(t.Context()))

	for _, tc := range []struct {
		name  string
		query string
		args  []any
	}{
		{"UPDATE журнала", `UPDATE ledger_entries SET reason = 'fixed' WHERE id = $1`, []any{in.ID}},
		{"DELETE журнала", `DELETE FROM ledger_entries WHERE id = $1`, []any{in.ID}},
		{"TRUNCATE журнала", `TRUNCATE ledger_entries`, nil},
		{"правка остатка", `UPDATE ledger_accounts SET balance_minor = 1000000 WHERE account = $1`, []any{account}},
		{"правка головы", `UPDATE ledger_accounts SET seq = 99, last_hash = NULL WHERE account = $1`, []any{account}},
		{"счёт с остатком", `INSERT INTO ledger_accounts (book, account, balance_minor) VALUES ($1, $2, 500)`,
			[]any{bookName, uuid.New()}},
		{"граница книги", `UPDATE ledger_books SET floor_minor = -1000000 WHERE book = $1`, []any{bookName}},
		{"знак рода", `UPDATE ledger_kinds SET sign = 'any' WHERE book = $1`, []any{bookName}},
	} {
		tx := beginAs(t, pool, role)
		_, execErr := tx.Exec(t.Context(), tc.query, tc.args...)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, execErr, &pgErr, tc.name)
		assert.Equal(t, "42501", pgErr.Code, "%s: отказ привилегией, а не триггером", tc.name)
		require.NoError(t, tx.Rollback(t.Context()))
	}

	head, err := store.Account(t.Context(), bookName, account)
	require.NoError(t, err)
	assert.Equal(t, int64(700), head.BalanceMinor, "остаток — ровно сумма движений")
	requireChain(t, svc, account, 2)
}

// appRole — роль без входа с правами из рецепта; тест заводит её сам и снимает
// за собой: роли общие на весь сервер, и на TEST_DATABASE_URL они пережили бы
// базу прогона.
func appRole(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	role := "ledger_app_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ident := pgx.Identifier{role}.Sanitize()
	var schema string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema))

	_, err := pool.Exec(t.Context(), "CREATE ROLE "+ident+" NOLOGIN")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+ident)
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident)
	})
	statements := append([]string{"GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO app;"}, appGrants...)
	for _, grant := range statements {
		_, err = pool.Exec(t.Context(), strings.Replace(grant, " TO app;", " TO "+ident, 1))
		require.NoError(t, err, grant)
	}
	return role
}

// beginAs — транзакция потребителя под ролью приложения: SET LOCAL ROLE
// снимается вместе с транзакцией, и соединение возвращается в пул владельцем.
func beginAs(t *testing.T, pool *pgxpool.Pool, role string) pgx.Tx {
	t.Helper()
	tx := beginTx(t, pool)
	_, err := tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	return tx
}

// Роль приложения без UPDATE на version не возьмёт даже блокировку: колонка —
// якорь привилегии, а не данные, и без неё движение невозможно в принципе.
func TestPrivileges_LockNeedsVersionGrant(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	role := appRole(t, pool)
	_, err := pool.Exec(t.Context(), "REVOKE UPDATE (version) ON ledger_accounts FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)

	svc := service(t, store)
	tx := beginAs(t, pool, role)
	_, err = svc.WithStore(store.WithTx(tx)).Post(t.Context(), topup(uuid.New(), 50, "no-version"))
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	var pgErr *pgconn.PgError
	assert.NotErrorAs(t, err, &pgErr, "граница адаптера")
	assert.Contains(t, err.Error(), "42501")
}
