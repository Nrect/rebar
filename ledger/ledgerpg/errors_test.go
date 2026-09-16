package ledgerpg_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// secretReason — «содержимое строки»: причину пишет оператор, в ней бывают
// персональные данные. В начале значения: Detail режет каждое поле до 64 байт.
const secretReason = "SECRET-7b1f: клиент Иванов просил вернуть"

// В Detail Postgres кладёт «Failing row contains (…)» — всю строку: причину,
// ключ клиента, суммы. Наружу этого не уходит ничего. Тест доказывает обе
// половины: что содержимое в ошибке базы ЕСТЬ, и что граница адаптера — тип
// *postgres.Error, а *pgconn.PgError через неё не проходит.
func TestStore_Error_DoesNotLeakRowContents(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, wallet())
	svc := service(t, store)
	account := uuid.New()
	mustPost(t, svc, topup(account, 1000, "leak-in"))
	// Триггер проверки снят: иначе он отказал бы раньше CHECK и без Detail, а
	// строку с Detail отдаёт именно CHECK.
	_, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_check_trg`)
	require.NoError(t, err)

	bad := nextEntry(t, store, account, kindAdjust, 10, "")
	bad.Reason = secretReason

	raw := insertRaw(t.Context(), pool, bad)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, raw, &pgErr)
	require.Contains(t, pgErr.Detail, "SECRET-7b1f", "контроль: иначе тест ниже ничего не доказывает")

	err = insertVia(t, store, bad)
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	assert.NotContains(t, err.Error(), "SECRET-7b1f")
	assert.NotContains(t, err.Error(), "Failing row")

	assert.NotErrorAs(t, err, &pgErr, "*pgconn.PgError не уезжает наружу")
	var clean *postgres.Error
	require.ErrorAs(t, err, &clean, "наружу едет очищенная ошибка postgres")
	assert.Equal(t, "23514", clean.Code)
	assert.Equal(t, "ledger_entries_key_chk", clean.Constraint)
	assert.NotContains(t, clean.Message, "SECRET-7b1f")
}
