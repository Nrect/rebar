package mailpg

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/postgres"
)

// Detail нарушения CHECK — «Failing row contains (…)» со всей строкой, включая
// тело письма: ни в тексте ошибки, ни в её цепочке его быть не должно. Имя
// ограничения, наоборот, обязано доехать: по нему потребитель отличает один
// конфликт от другого.
func TestStoreError_FoldsPgErrorWithoutDetail(t *testing.T) {
	t.Parallel()

	const body = "Ссылка: https://example.ru/verify?token=SECRET-TOKEN-42"
	pgErr := &pgconn.PgError{
		Severity:       "ERROR",
		Code:           "23514",
		Message:        `new row for relation "email_outbox" violates check constraint "email_outbox_fail_reason_chk"`,
		ConstraintName: "email_outbox_fail_reason_chk",
		Detail:         "Failing row contains (7b1c…, verify, " + body + ", …).",
	}

	err := storeError("finish", pgErr)

	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.Contains(t, err.Error(), "23514")
	assert.Contains(t, err.Error(), "violates check constraint")
	assert.NotContains(t, err.Error(), body)
	assert.NotContains(t, err.Error(), "Failing row")

	var unwrapped *pgconn.PgError
	assert.NotErrorAs(t, err, &unwrapped, "*PgError в цепочке отдал бы Detail через errors.As")

	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "наружу едет очищенная ошибка общей границы")
	assert.Equal(t, "email_outbox_fail_reason_chk", sanitized.Constraint)
	assert.NotContains(t, sanitized.Message, body)
}

// Сетевой сбой и отмена ctx остаются видимыми: по ним вызывающий отличает
// «база недоступна» от «мы сами отменили».
func TestStoreError_KeepsPlainErrorInChain(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset by peer")

	err := storeError("claim", sentinel)

	require.ErrorIs(t, err, mail.ErrUnavailable)
	assert.ErrorIs(t, err, sentinel)
}

func TestStoreError_NilStaysNil(t *testing.T) {
	t.Parallel()
	assert.NoError(t, storeError("stats", nil))
}
