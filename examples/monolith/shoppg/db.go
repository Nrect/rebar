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
const (
	uxUsersLogin = "ux_shop_users_login"
	uxGrants     = "ux_entitlement_grants_subject_item"
)

// DB — пул и раннер транзакций потребителя. Всё, что ниже, работает либо на
// пуле, либо на транзакции, полученной снаружи: своей транзакции адаптеры не
// открывают, иначе «в одной транзакции с бизнес-фактом» было бы пожеланием.
type DB struct {
	Pool   *pgxpool.Pool
	Runner *postgres.Runner
}

// Open поднимает пул и раннер. Зона соединения пинуется в UTC: дата-арифметика
// на сервере перестаёт зависеть от того, в каком образе он поднят.
func Open(ctx context.Context, dsn string, cfg postgres.Config) (*DB, error) {
	utc, err := postgres.WithUTC(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, utc)
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
