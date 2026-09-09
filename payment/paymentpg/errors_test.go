package paymentpg_test

import (
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/postgres"
)

// В Detail Postgres кладёт «Failing row contains (…)» — всю строку целиком:
// сумму, ссылку потребителя, ключ идемпотентности. Наружу этого не уходит
// ничего: остаются SQLSTATE, Message и имя ограничения.
//
// Тест доказывает обе половины: что содержимое строки в ошибке базы ЕСТЬ (иначе
// он проверял бы пустоту) и что через границу адаптера оно не проходит.
func TestStore_Error_DoesNotLeakRowContents(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	const secretAmount int64 = 1234567
	bad := intent(func(in *payment.Intent) {
		in.Reference = secretReference
		in.AmountMinor = secretAmount
		in.Items = []payment.OrderItem{
			{Position: 0, ProductID: "sku-secret", Title: "Курс", AmountMinor: secretAmount, Quantity: 1},
		}
		in.Currency = "RU" // валюта не по форме: CHECK отвергнет строку
	})

	// Сырая ошибка Postgres на той же вставке: в ней содержимое строки есть.
	raw := insertIntentDirectly(t, pool, bad)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, raw, &pgErr)
	require.Contains(t, pgErr.Detail, secretReference, "иначе тест ниже ничего не доказывает")
	require.Contains(t, pgErr.Detail, strconv.FormatInt(secretAmount, 10))

	err := store.CreateIntent(t.Context(), bad)
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Contains(t, err.Error(), "payment_intents_currency_chk", "имя ограничения — это имя схемы")
	assert.NotContains(t, err.Error(), secretReference)
	assert.NotContains(t, err.Error(), strconv.FormatInt(secretAmount, 10))
	assert.NotContains(t, err.Error(), "Failing row")
	assert.NotContains(t, err.Error(), "sku-secret")

	// ПРОВЕРКА ПО ТИПУ, А НЕ ПО ТЕКСТУ: *pgconn.PgError не заворачивается в
	// цепочку (иначе Detail достался бы через errors.As ниже по стеку, где о
	// нём уже никто не думает), а вместо него едет очищенный *postgres.Error —
	// по нему и разбирают конфликт по имени.
	assert.NotErrorAs(t, err, &pgErr, "*pgconn.PgError не уезжает наружу")
	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "наружу едет очищенная ошибка postgres")
	assert.Equal(t, "payment_intents_currency_chk", sanitized.Constraint)
	assert.NotContains(t, sanitized.Message, secretReference)

	// То же на пути книги: там в строке лежат суммы.
	in := mustCreate(t, store, intent())
	capture := settleIntent(t, store, in)
	dup := capture
	dup.ID = uuid.New()
	dup.AmountMinor = secretAmount
	_, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: dup, Now: testNow(),
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), strconv.FormatInt(secretAmount, 10))
}

// insertIntentDirectly — та же вставка мимо адаптера: нужна, чтобы увидеть
// ошибку Postgres до Sanitize.
func insertIntentDirectly(t *testing.T, pool *pgxpool.Pool, in payment.Intent) error {
	t.Helper()
	_, err := pool.Exec(t.Context(), `INSERT INTO payment_intents (id, payer_id, reference,
		amount_minor, currency, provider, method, auto_capture, provider_payment_id,
		confirmation_type, confirmation_url, confirmation_qr, status, idempotency_key,
		params_fingerprint, created_at, updated_at, expires_at, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'','','','',$9,$10,$11,$12,$13,$14,NULL)`,
		in.ID, in.PayerID, in.Reference, in.AmountMinor, in.Currency, string(in.Provider),
		string(in.Method), in.AutoCapture, string(in.Status), in.IdempotencyKey,
		in.ParamsFingerprint, in.CreatedAt, in.UpdatedAt, in.ExpiresAt)
	require.Error(t, err)
	return err
}
