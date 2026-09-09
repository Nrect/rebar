package authpg

import (
	"fmt"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/postgres"
)

// storeError — единственная точка перевода сбоя Postgres в ошибку порта:
// errors.Is(err, auth.ErrUnavailable) означает 503, а не «неверные данные».
//
// ХЭШ ПАРОЛЯ И ХЭШ ТОКЕНА НЕ ПОПАДАЮТ В ОШИБКУ. Границу держит
// postgres.Sanitize: у pgconn.PgError в Detail лежит «Failing row contains
// (…)» — вся строка целиком, а строки этих таблиц состоят из ключей сессий и
// нормализованных логинов. Поэтому *PgError не заворачивается в цепочку
// (иначе Detail достаётся через errors.As ниже по стеку), и от ошибки
// Postgres остаются SQLSTATE, Message и имя ограничения.
//
// Граница общая, а не скопированная: пять адаптеров с пятью её копиями — это
// пять шансов разойтись ровно там, где расхождение стоит утечки содержимого
// строки (ADR-0005, «Межмодульные зависимости»).
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: authpg: %s: %w", auth.ErrUnavailable, op, postgres.Sanitize(err))
}
