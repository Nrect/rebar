package paymentpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, payment.ErrUnavailable) означает «решение не принято, деньги
// не двинулись», то есть 503 и повтор провайдера.
//
// ГРАНИЦА ОШИБКИ — ОБЩАЯ, А НЕ СВОЯ КОПИЯ (ADR-0005, третья межмодульная
// зависимость). У pgconn.PgError в Detail лежит «Failing row contains (…)» —
// вся строка целиком: сумма, ссылка потребителя, состав заказа;
// postgres.Sanitize оставляет от неё SQLSTATE, Message и имя ограничения и не
// заворачивает *PgError в цепочку, поэтому Detail не достанется и через
// errors.As ниже по стеку.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: paymentpg: %s: %w", payment.ErrUnavailable, op, postgres.Sanitize(err))
}

// raisedBy — база отбила запись ИМЕНОВАННЫМ ограничением.
//
// Дополняет postgres.IsUniqueViolation, а не заменяет: та закрывает 23505, то
// есть индексы, а инварианты книги держат триггеры и приезжают как 23514 с тем
// же полем имени (RAISE … USING CONSTRAINT). Разбор всё равно ПО ИМЕНИ, а не по
// коду: у двух нарушений в одной таблице исходы бывают противоположными.
//
// Работает и до Sanitize (*pgconn.PgError), и после (*postgres.Error): иначе
// ошибка, прошедшая границу адаптера, молча переставала бы опознаваться.
// Пустое имя — всегда false: Postgres называет ограничение не всегда.
func raisedBy(err error, constraint string) bool {
	if constraint == "" {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName == constraint
	}
	var sanitized *postgres.Error
	if errors.As(err, &sanitized) {
		return sanitized.Constraint == constraint
	}
	return false
}
