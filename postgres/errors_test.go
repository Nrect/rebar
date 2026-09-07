package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres"
)

// secret — «содержимое строки», которое Postgres кладёт в Detail: ищем его в
// текстах ошибок.
const secret = "https://example.ru/verify?token=SECRET-TOKEN-42"

func pgErr(code, constraint string) *pgconn.PgError {
	return &pgconn.PgError{
		Severity:       "ERROR",
		Code:           code,
		Message:        `duplicate key value violates unique constraint "` + constraint + `"`,
		Detail:         "Failing row contains (7b1c…, verify, " + secret + ", …).",
		Hint:           "Смотри " + secret,
		Where:          "PL/pgSQL function f() line 1 at SQL statement",
		ConstraintName: constraint,
	}
}

func TestSanitize_DropsDetail(t *testing.T) {
	t.Parallel()

	err := postgres.Sanitize(fmt.Errorf("сохранение заказа: %w", pgErr("23505", "ux_orders_idem")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "SQLSTATE 23505")
	assert.Contains(t, err.Error(), "violates unique constraint")
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), "Failing row")
	assert.NotContains(t, err.Error(), "PL/pgSQL")

	var leaked *pgconn.PgError
	assert.NotErrorAs(t, err, &leaked, "*PgError в цепочке отдал бы Detail через errors.As")
}

func TestSanitize_KeepsOtherErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset by peer")
	wrapped := fmt.Errorf("пул: %w", sentinel)

	require.NoError(t, postgres.Sanitize(nil))
	assert.Equal(t, wrapped, postgres.Sanitize(wrapped), "не-PgError проходит как есть")
	assert.ErrorIs(t, postgres.Sanitize(wrapped), sentinel)
}

// Классификация обязана пережить Sanitize: адаптер зовёт InTx, а он чистит
// ошибку до возврата.
func TestClassifiers_WorkAfterSanitize(t *testing.T) {
	t.Parallel()

	raw := pgErr("23505", "ux_orders_idem")
	clean := postgres.Sanitize(raw)

	assert.True(t, postgres.IsUniqueViolation(clean, "ux_orders_idem"))
	assert.True(t, postgres.IsRetryable(postgres.Sanitize(pgErr("40001", ""))))
	assert.True(t, postgres.IsContention(postgres.Sanitize(pgErr("55P03", ""))))
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "40001 serialization_failure", err: pgErr("40001", ""), want: true},
		{name: "40P01 deadlock_detected", err: pgErr("40P01", ""), want: true},
		{name: "55P03 lock_not_available — ждать, а не повторять", err: pgErr("55P03", "")},
		{name: "57014 query_canceled — бюджет съеден", err: pgErr("57014", "")},
		{name: "23505 unique_violation", err: pgErr("23505", "ux")},
		{name: "не ошибка Postgres", err: errors.New("connection reset by peer")},
		{name: "nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, postgres.IsRetryable(tt.err))
		})
	}
}

func TestIsContention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "55P03 lock_not_available — сюда приходит lock_timeout", err: pgErr("55P03", ""), want: true},
		{name: "57014 query_canceled — это statement_timeout, не блокировка", err: pgErr("57014", "")},
		{name: "40P01 deadlock_detected", err: pgErr("40P01", "")},
		{name: "не ошибка Postgres", err: errors.New("connection reset by peer")},
		{name: "nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, postgres.IsContention(tt.err))
		})
	}
}

func TestIsUniqueViolation_ByName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		constraint string
		want       bool
	}{
		{name: "тот самый индекс", err: pgErr("23505", "ux_orders_idem"), constraint: "ux_orders_idem", want: true},
		{name: "чужой UNIQUE в той же таблице", err: pgErr("23505", "ux_orders_number"), constraint: "ux_orders_idem"},
		{name: "не 23505", err: pgErr("23514", "ux_orders_idem"), constraint: "ux_orders_idem"},
		{name: "пустое имя не совпадает ни с чем", err: pgErr("23505", ""), constraint: ""},
		{name: "Postgres не назвал constraint", err: pgErr("23505", ""), constraint: "ux_orders_idem"},
		{name: "не ошибка Postgres", err: errors.New("connection reset by peer"), constraint: "ux_orders_idem"},
		{name: "nil", constraint: "ux_orders_idem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, postgres.IsUniqueViolation(tt.err, tt.constraint))
		})
	}
}
