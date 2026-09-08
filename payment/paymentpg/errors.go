package paymentpg

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nrect/rebar/payment"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, payment.ErrUnavailable) означает «решение не принято, деньги
// не двинулись», то есть 503 и повтор провайдера.
//
// СОДЕРЖИМОЕ СТРОКИ НЕ ПОПАДАЕТ В ОШИБКУ. У pgconn.PgError в Detail лежит
// «Failing row contains (…)» — вся строка целиком: сумма, ссылка потребителя,
// состав. Поэтому *PgError не заворачивается в цепочку (иначе Detail достался
// бы через errors.As ниже по стеку), от него остаются SQLSTATE, Message и имя
// ограничения — это имена схемы, а не данные.
//
// Копия правила postgres.Sanitize, а не импорт: ADR-0005 разрешает адаптеру
// зависеть от модуля postgres только из _test.go.
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%w: paymentpg: %s: SQLSTATE %s: %s",
			payment.ErrUnavailable, op, pgErr.Code, pgErr.Message)
	}
	return fmt.Errorf("%w: paymentpg: %s: %w", payment.ErrUnavailable, op, err)
}

// violates — нарушено ИМЕННО это ограничение.
//
// ПО ИМЕНИ, А НЕ ПО КОДУ: в таблице намерений два уникальных ограничения, и у
// них противоположные исходы — занятый ключ идемпотентности это повтор, занятая
// ссылка это отказ. «Любое 23505 — дубль» вернуло бы клиенту чужую ссылку на
// оплату. По той же причине триггеры книги поднимают ошибку с именем
// ограничения (USING CONSTRAINT), а не с одним кодом.
//
// Пустое имя — всегда false: Postgres называет ограничение не всегда, и «»
// совпало бы с этим.
func violates(err error, constraint string) bool {
	if constraint == "" {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName == constraint
	}
	return false
}
