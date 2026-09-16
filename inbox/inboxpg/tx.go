package inboxpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// abortSQL — заведомо падающий запрос, прерывающий транзакцию потребителя после
// отказа (уточнение арбитра 1). Код 25P02 postgres.IsRetryable не повторяет, а
// текст называет, кто и почему прервал: трассировщик pgx и журнал сервера
// покажут его, а не фантомную ошибку SQL.
const abortSQL = `DO $$ BEGIN RAISE EXCEPTION 'inboxpg: транзакция прервана после отказа' USING ERRCODE = '25P02'; END $$`

// txActive — pgconn.TxStatus транзакции, в которой запросы ещё исполняются.
const txActive byte = 'T'

// cleanupTimeout — бюджет отката и прерывания мимо отмены ctx: не уложились —
// pgx рвёт соединение, и транзакция не закоммитится и так.
const cleanupTimeout = 5 * time.Second

// inTx проводит fn через одну транзакцию: свою из пула либо потребителя (WithTx).
func inTx[T any](ctx context.Context, s *Store, op string, fn func(tx pgx.Tx) (T, error)) (T, error) {
	var zero T
	if s.tx != nil {
		return inConsumerTx(ctx, s.tx, fn)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, storeError(op+": begin", err)
	}
	// Откат и на панике обработчика; после коммита — пустой ход.
	defer rollback(ctx, tx)
	res, err := fn(tx)
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, storeError(op+": commit", err)
	}
	return res, nil
}

// inConsumerTx — fn в транзакции потребителя: ошибка и паника её прерывают.
func inConsumerTx[T any](ctx context.Context, tx pgx.Tx, fn func(tx pgx.Tx) (T, error)) (res T, err error) {
	failed := true
	defer func() {
		if failed {
			abort(ctx, tx)
		}
	}()
	res, err = fn(tx)
	failed = err != nil
	return res, err
}

// refuse — отказ до первого запроса: в режиме WithTx транзакция потребителя
// прерывается и здесь, любая ошибка — одно правило.
func (s *Store) refuse(ctx context.Context, err error) error {
	if s.tx != nil {
		abort(ctx, s.tx)
	}
	return err
}

// abort прерывает транзакцию мимо отмены ctx: по отменённому pgx запрос не
// отправил бы. Прерванную базой или оставшуюся без соединения не трогает —
// причина в ней уже есть.
func abort(ctx context.Context, tx pgx.Tx) {
	if tx.Conn().PgConn().TxStatus() != txActive {
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = tx.Exec(detached, abortSQL)
}

// rollback — откат мимо отмены ctx: по отменённому pgx не отправил бы ROLLBACK,
// а закрыл бы соединение. Ошибка не возвращается — наружу уже уходит своя.
func rollback(ctx context.Context, tx pgx.Tx) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = tx.Rollback(detached)
}
