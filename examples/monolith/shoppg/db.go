package shoppg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/postgres"
)

// Имена ограничений, которые разбирает код. Это контракт схемы: переименование
// в миграции ломает разбор молча (CONVENTIONS §9).
const uxUsersLogin = "ux_shop_users_login"

// DB — пул и раннер транзакций потребителя. Всё, что ниже, работает либо на
// пуле, либо на транзакции, полученной снаружи: своей транзакции адаптеры не
// открывают, иначе «в одной транзакции с бизнес-фактом» было бы пожеланием.
type DB struct {
	Pool   *pgxpool.Pool
	Runner *postgres.Runner
}

// idleInTxTimeout — срок забытой открытой транзакции: она держит блокировки и
// не даёт VACUUM убрать строки. Транзакции примера не ждут ничего, кроме базы, и
// простой в минуту — это ручка, открывшая транзакцию и не закрывшая её.
const idleInTxTimeout = "60s"

// Open поднимает пул и раннер. Зона соединения пинуется в UTC: дата-арифметика
// на сервере перестаёт зависеть от того, в каком образе он поднят. service —
// application_name: в pg_stat_activity видно, чей запрос держит блокировку.
// Сроки запросов в DSN не кладутся: их ставит Runner (postgres/doc.go, п. 2).
func Open(ctx context.Context, dsn, service string, cfg postgres.Config) (*DB, error) {
	pinned, err := poolDSN(dsn, service)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, pinned)
	if err != nil {
		// Текст DSN в ошибку не попадает: в нём пароль, а ошибка старта почти
		// всегда оказывается в логе.
		return nil, errors.New("shoppg: пул не поднялся")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("shoppg: база не отвечает")
	}
	return &DB{Pool: pool, Runner: postgres.New(pool, cfg)}, nil
}

// poolDSN — DSN пула: UTC, имя сервиса и срок забытой транзакции.
func poolDSN(dsn, service string) (string, error) {
	out, err := postgres.WithUTC(dsn)
	if err != nil {
		return "", err
	}
	if out, err = postgres.WithRuntimeParam(out, "application_name", service); err != nil {
		return "", err
	}
	return postgres.WithRuntimeParam(out, "idle_in_transaction_session_timeout", idleInTxTimeout)
}

// Close закрывает пул. Идемпотентен на стороне pgxpool.
func (db *DB) Close() { db.Pool.Close() }

// storeError — единственная граница ошибки адаптера. postgres.Sanitize
// снимает Detail; своей копии границы здесь нет намеренно (doc.go, п. 1).
func storeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("shoppg: %s: %w", op, postgres.Sanitize(err))
}

// utc — момент так, как его хранит timestamptz. pgx отдаёт время в зоне
// соединения, а порты говорят о моментах.
func utc(t time.Time) time.Time { return t.UTC() }

// noRows — «строки нет», а не сбой. Отдельной функцией, чтобы ветка читалась
// одинаково во всех адаптерах пакета.
func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
