package ledgerpg_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgerpg"
)

// CheckSchema сверяет, но не применяет: на пустой схеме он называет каждую
// таблицу и говорит, что делать, а таблиц не заводит.
func TestCheckSchema_MissingTablesTellWhatToDo(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	err := ledgerpg.New(pool, wallet()).CheckSchema(t.Context())
	require.Error(t, err)
	for _, table := range []string{"ledger_accounts", "ledger_books", "ledger_entries", "ledger_kinds"} {
		assert.Contains(t, err.Error(), "ledgerpg.CheckSchema: таблицы "+table+" нет: накатите ledgerpg.Migrations() раннером проекта")
	}
	require.NotErrorIs(t, err, ledger.ErrUnavailable, "расхождение схемы — не временный сбой")
	assert.Empty(t, schemaObjects(t, pool), "CheckSchema ничего не применяет")
}

// Каждое расхождение — строкой с именем объекта, первая строка — что делать.
// Лишних строк нет: расхождение не маскирует соседнее и не множится.
func TestCheckSchema_ReportsEveryMismatchByName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ddl  []string
		want []string
	}{
		{
			name: "нет индекса одной отмены",
			ddl:  []string{`DROP INDEX ux_ledger_entries_reversal`},
			want: []string{"ledger_entries: индекса ux_ledger_entries_reversal нет"},
		},
		{
			name: "нет CHECK и внешнего ключа",
			ddl: []string{
				`ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_amount_chk`,
				`ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_kind_fkey`,
			},
			want: []string{
				"ledger_entries: ограничения ledger_entries_amount_chk нет",
				"ledger_entries: ограничения ledger_entries_kind_fkey нет",
			},
		},
		{
			name: "нет колонки и другой тип",
			ddl: []string{
				`ALTER TABLE ledger_accounts DROP COLUMN version`,
				`ALTER TABLE ledger_books ALTER COLUMN floor_minor TYPE NUMERIC`,
			},
			want: []string{
				"ledger_accounts: колонки version нет",
				"ledger_books: колонка floor_minor имеет тип numeric, ожидается bigint",
			},
		},
		{
			name: "триггер выключен",
			ddl:  []string{`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_immutable_trg`},
			want: []string{"ledger_entries: триггер ledger_entries_immutable_trg не ENABLE ALWAYS — он молчит при репликации"},
		},
		{
			// Режим по умолчанию: триггер на месте и молчит на реплике.
			name: "триггер в режиме ORIGIN",
			ddl:  []string{`ALTER TABLE ledger_accounts ENABLE TRIGGER ledger_accounts_guard_trg`},
			want: []string{"ledger_accounts: триггер ledger_accounts_guard_trg не ENABLE ALWAYS — он молчит при репликации"},
		},
		{
			// DROP + CREATE без явного ALTER приходит в ORIGIN (ADR-0011, решение 4).
			name: "триггер пересоздан без ENABLE ALWAYS",
			ddl: []string{
				`DROP TRIGGER ledger_entries_check_trg ON ledger_entries`,
				`CREATE TRIGGER ledger_entries_check_trg BEFORE INSERT ON ledger_entries
					FOR EACH ROW EXECUTE FUNCTION ledger_entries_check()`,
			},
			want: []string{"ledger_entries: триггер ledger_entries_check_trg не ENABLE ALWAYS — он молчит при репликации"},
		},
		{
			name: "нет триггера",
			ddl:  []string{`DROP TRIGGER ledger_entries_no_truncate_trg ON ledger_entries`},
			want: []string{"ledger_entries: триггера ledger_entries_no_truncate_trg нет"},
		},
		{
			name: "запись остатка без прав владельца",
			ddl:  []string{`ALTER FUNCTION ledger_entries_apply() SECURITY INVOKER`},
			want: []string{"ledger_entries: функция ledger_entries_apply не SECURITY DEFINER — роль приложения не запишет остаток"},
		},
		{
			name: "search_path функции не закреплён",
			ddl:  []string{`ALTER FUNCTION ledger_accounts_guard() RESET search_path`},
			// Функцию зовут два триггера, а расхождение — одно.
			want: []string{"ledger_accounts: у функции ledger_accounts_guard search_path не закреплён с pg_temp последней"},
		},
		{
			name: "триггер зовёт чужую функцию",
			ddl: []string{
				`DROP TRIGGER ledger_entries_immutable_trg ON ledger_entries`,
				`CREATE TRIGGER ledger_entries_immutable_trg BEFORE UPDATE OR DELETE ON ledger_entries
					FOR EACH ROW EXECUTE FUNCTION ledger_accounts_guard()`,
				`ALTER TABLE ledger_entries ENABLE ALWAYS TRIGGER ledger_entries_immutable_trg`,
			},
			want: []string{"ledger_entries: триггер ledger_entries_immutable_trg зовёт функцию ledger_accounts_guard, ожидается ledger_entries_immutable"},
		},
		{
			name: "граница и единица книги разошлись с реестром",
			ddl:  []string{`UPDATE ledger_books SET floor_minor = -500, unit = 'points' WHERE book = 'wallet'`},
			want: []string{
				"ledger_books: у книги wallet единица points, в реестре RUB",
				"ledger_books: у книги wallet граница -500, в реестре 0",
			},
		},
		{
			name: "род разошёлся, пропал и лишний",
			ddl: []string{
				`UPDATE ledger_kinds SET sign = 'any' WHERE kind = 'topup'`,
				`DELETE FROM ledger_kinds WHERE kind = 'adjustment'`,
				`INSERT INTO ledger_kinds VALUES ('wallet', 'bonus', 'credit', 'optional', 'optional')`,
			},
			want: []string{
				"ledger_kinds: у рода wallet/topup в справочнике знак any, основание required, причина и автор optional, " +
					"в реестре знак credit, основание required, причина и автор optional",
				"ledger_kinds: рода wallet/adjustment нет, в реестре знак any, основание optional, причина и автор required",
				"ledger_kinds: рода wallet/bonus нет в реестре книги",
			},
		},
		{
			name: "книги нет в справочнике",
			ddl:  []string{`DELETE FROM ledger_kinds`, `DELETE FROM ledger_books`},
			// Миграцию справочника пишут по сообщению: книга и все её роды со значениями.
			want: []string{
				"ledger_books: книги wallet нет, в реестре единица RUB, граница 0",
				"ledger_kinds: рода wallet/topup нет, в реестре знак credit, основание required, причина и автор optional",
				"ledger_kinds: рода wallet/spend нет, в реестре знак debit, основание required, причина и автор optional",
				"ledger_kinds: рода wallet/adjustment нет, в реестре знак any, основание optional, причина и автор required",
				"ledger_kinds: рода wallet/reversal нет, в реестре знак any, основание optional, причина и автор required",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t, wallet())
			for _, ddl := range tt.ddl {
				_, err := pool.Exec(t.Context(), ddl)
				require.NoError(t, err, ddl)
			}

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			assert.Equal(t, "ledgerpg.CheckSchema: схема расходится с миграциями — накатите ledgerpg.Migrations() "+
				"раннером проекта; что осталось после наката, чините своей миграцией", lines[0], "первая строка — что делать")
			assert.ElementsMatch(t, tt.want, lines[1:], "расхождения по именам и без лишних")
			assert.NotErrorIs(t, err, ledger.ErrUnavailable)
		})
	}
}

// Потребитель вправе расширять таблицы своими колонками и индексами, а книга,
// которой нет в New, CheckSchema не касается: у другого сервиса свой реестр.
func TestCheckSchema_IgnoresConsumerAdditions(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	points := wallet()
	points.Name, points.Unit = "points", "points"
	seedBook(t, pool, points)
	for _, ddl := range []string{
		`ALTER TABLE ledger_entries ADD COLUMN tenant_id UUID`,
		`CREATE INDEX ix_ledger_entries_tenant ON ledger_entries (tenant_id)`,
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err, ddl)
	}
	require.NoError(t, store.CheckSchema(t.Context()))

	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 100, "extra-column"))
	requireChain(t, svc, account, 1)
}

// Сбой каталога — временный: ledger.ErrUnavailable, как у любого запроса адаптера.
func TestCheckSchema_CatalogFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	pool.Close()
	require.ErrorIs(t, store.CheckSchema(t.Context()), ledger.ErrUnavailable)
}
