package idempg_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/postgres"
)

// secretLocation — «содержимое строки»: ответ несёт то, что вернула ручка. В
// начале значения: Detail режет каждое поле до 64 байт.
const secretLocation = "/SECRET-7b1f/orders/1"

// В Detail Postgres кладёт «Failing row contains (…)» — всю запись: область,
// ключ, ответ. Наружу этого не уходит ничего. Тест доказывает обе половины:
// что содержимое в ошибке базы ЕСТЬ, и что граница адаптера — тип
// *postgres.Error, а *pgconn.PgError через неё не проходит.
func TestStore_Error_DoesNotLeakRowContents(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	// Свой CHECK потребителя строже ядра: вставка записи падает с Detail.
	_, err := pool.Exec(t.Context(),
		`ALTER TABLE idem_records ADD CONSTRAINT shop_records_location_chk CHECK (location NOT LIKE '%SECRET%')`)
	require.NoError(t, err)
	req := request(t, "leak")
	resp := created(1)
	resp.Location = secretLocation

	var pgErr *pgconn.PgError
	raw, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, rawErr := raw.Exec(t.Context(), `INSERT INTO idem_records
		(realm, subject, idem_key, operation, fingerprint, status, content_type, location, body, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		req.Scope.Realm, req.Scope.Subject, req.Key.String(), string(req.Operation), req.Fingerprint(),
		resp.Status, resp.ContentType, resp.Location, resp.Body, testNow)
	require.NoError(t, raw.Rollback(t.Context()))
	require.ErrorAs(t, rawErr, &pgErr)
	require.Contains(t, pgErr.Detail, "SECRET-7b1f", "контроль: иначе тест ниже ничего не доказывает")

	_, err = store.Do(t.Context(), req, order("leak", resp))
	require.ErrorIs(t, err, idem.ErrUnavailable)
	assert.NotContains(t, err.Error(), "SECRET-7b1f")
	assert.NotContains(t, err.Error(), "Failing row")

	assert.NotErrorAs(t, err, &pgErr, "*pgconn.PgError не уезжает наружу")
	var clean *postgres.Error
	require.ErrorAs(t, err, &clean, "наружу едет очищенная ошибка postgres")
	assert.Equal(t, "23514", clean.Code)
	assert.Equal(t, "shop_records_location_chk", clean.Constraint)
	assert.NotContains(t, clean.Message, "SECRET-7b1f")
}
