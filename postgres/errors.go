package postgres

import (
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE, по которым принимаются решения. Коды, а не тексты: тексты
// локализуются настройкой сервера lc_messages.
const (
	codeSerializationFailure = "40001" // конфликт сериализации
	codeDeadlockDetected     = "40P01" // взаимная блокировка
	codeLockNotAvailable     = "55P03" // сюда же приходит истёкший lock_timeout
	codeQueryCanceled        = "57014" // statement_timeout или отмена извне
	codeUniqueViolation      = "23505"
)

// Error — ошибка Postgres, из которой убрано содержимое строки.
//
// DETAIL/HINT/WHERE НЕ ХРАНЯТСЯ. В Detail Postgres кладёт «Failing row
// contains (…)» — всю строку целиком: хэш пароля, тело письма, токен. Наружу
// отдаются SQLSTATE, Message и имя constraint (это имена схемы, не данные), и
// этого хватает классификаторам ниже: они работают и после Sanitize.
type Error struct {
	Code       string // SQLSTATE
	Message    string
	Constraint string // ConstraintName, если Postgres его назвал
}

func (e *Error) Error() string { return "SQLSTATE " + e.Code + ": " + e.Message }

// Sanitize — граница адаптера: ошибка Postgres теряет Detail, остальные
// проходят как есть. nil остаётся nil.
//
// *pgconn.PgError НЕ ЗАВОРАЧИВАЕТСЯ В ЦЕПОЧКУ: иначе Detail достался бы через
// errors.As ниже по стеку, где о нём уже никто не думает.
func Sanitize(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return &Error{Code: pgErr.Code, Message: pgErr.Message, Constraint: pgErr.ConstraintName}
	}
	return err
}

// IsRetryable — сбой, который повторяется сам собой: конфликт сериализации
// (40001) и дедлок (40P01). Отменённый по таймауту запрос (57014) сюда не
// входит: бюджет уже съеден, повтор съест ещё один.
func IsRetryable(err error) bool {
	switch code(err) {
	case codeSerializationFailure, codeDeadlockDetected:
		return true
	default:
		return false
	}
}

// IsContention — блокировку взять не удалось (55P03): истёк lock_timeout либо
// сработал NOWAIT. Postgres даёт на это именно 55P03, а не 57014, — 57014
// означает statement_timeout или отмену извне и повторяться не должен.
//
// Решение по 55P03 принимает вызывающий: очередь может подождать, ответ
// пользователю — нет.
func IsContention(err error) bool { return code(err) == codeLockNotAvailable }

// IsUniqueViolation — нарушение ИМЕННО этого уникального индекса.
//
// ПО ИМЕНИ, А НЕ ПО 23505: в таблице обычно не один UNIQUE, и «повтором по
// ключу идемпотентности» нельзя объявлять чужой конфликт. Пустое имя — всегда
// false: Postgres не всегда называет constraint, и «» совпало бы с этим.
func IsUniqueViolation(err error, constraint string) bool {
	if constraint == "" {
		return false
	}
	c, name := codeAndConstraint(err)
	return c == codeUniqueViolation && name == constraint
}

func code(err error) string {
	c, _ := codeAndConstraint(err)
	return c
}

// codeAndConstraint — классификация работает и до Sanitize (*pgconn.PgError),
// и после (*Error): иначе ошибка, прошедшая границу адаптера, молча переставала
// бы быть «повтором по ключу».
func codeAndConstraint(err error) (sqlstate, constraint string) {
	if err == nil {
		return "", ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	var sanitized *Error
	if errors.As(err, &sanitized) {
		return sanitized.Code, sanitized.Constraint
	}
	return "", ""
}

// attemptsError — «не вышло за N попыток» отдельным типом, чтобы номер попытки
// не собирался из строк на каждом вызове.
type attemptsError struct {
	attempts int
	err      error
}

func (e *attemptsError) Error() string {
	return "postgres: after " + strconv.Itoa(e.attempts) + " attempts: " + e.err.Error()
}

func (e *attemptsError) Unwrap() error { return e.err }
