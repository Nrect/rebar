package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxFunc — тело транзакции. ctx тот же, что у Runner: отмена доводит до
// rollback, а не оставляет транзакцию открытой.
type TxFunc func(ctx context.Context, tx pgx.Tx) error

// Runner — транзакции с таймаутами и повторами поверх пула потребителя.
type Runner struct {
	pool     *pgxpool.Pool
	cfg      Config
	setLocal string
}

// New паникует на nil-пуле и негодном Config: и то и другое — ошибка сборки
// приложения, её место на старте (как mail.NewService).
func New(pool *pgxpool.Pool, cfg Config) *Runner {
	if pool == nil {
		panic("postgres.New: nil pool")
	}
	cfg.validate()
	return &Runner{pool: pool, cfg: cfg, setLocal: setLocalStmt(cfg)}
}

// InTx проводит fn через транзакцию: BeginTx → SET LOCAL таймауты → fn →
// commit или rollback. Ошибка наружу проходит через Sanitize — потребитель
// никогда не увидит Detail.
func (r *Runner) InTx(ctx context.Context, fn TxFunc) error {
	return Sanitize(r.once(ctx, fn))
}

// InTxRetry — то же, но с повтором по IsRetryable (40001, 40P01).
//
// ТОЛЬКО ДЛЯ ИДЕМПОТЕНТНЫХ fn: тело исполняется заново целиком, вместе со
// всеми своими побочными эффектами вне базы. Отправка письма или списание
// внутри fn при повторе случится дважды.
func (r *Runner) InTxRetry(ctx context.Context, fn TxFunc) error {
	var last error
	for attempt := 1; ; attempt++ {
		last = r.once(ctx, fn)
		if last == nil {
			return nil
		}
		if !IsRetryable(last) {
			return Sanitize(last)
		}
		if attempt >= r.cfg.MaxAttempts {
			return &attemptsError{attempts: r.cfg.MaxAttempts, err: Sanitize(last)}
		}
		if err := sleep(ctx, backoff(r.cfg.RetryBase, attempt, rand.Float64())); err != nil { //nolint:gosec // джиттер повтора, не секрет
			return errors.Join(Sanitize(last), err)
		}
	}
}

// once — одна транзакция целиком; ошибка возвращается сырой, Sanitize зовут
// вызывающие (классификация повтора смотрит на *pgconn.PgError).
func (r *Runner) once(ctx context.Context, fn TxFunc) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	// ПАНИКА В fn — ОТКАТ И ПАНИКА ДАЛЬШЕ. Без этого транзакция уехала бы в
	// пул открытой и держала бы блокировки до конца жизни соединения.
	defer func() {
		if p := recover(); p != nil {
			_ = rollback(ctx, tx)
			panic(p)
		}
	}()
	if _, err := tx.Exec(ctx, r.setLocal); err != nil {
		return Finish(ctx, tx, err)
	}
	return Finish(ctx, tx, fn(ctx, tx))
}

// Finish завершает транзакцию: commit при err == nil, rollback иначе. Ошибка
// rollback не подменяет исходную, а присоединяется к ней (errors.Join):
// потерять причину отката хуже, чем прочитать две строки.
func Finish(ctx context.Context, tx pgx.Tx, err error) error {
	if err != nil {
		if rbErr := rollback(ctx, tx); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return tx.Commit(ctx)
}

// rollback идёт МИМО ОТМЕНЫ ctx (context.WithoutCancel): по отменённому ctx
// pgx не отправил бы ROLLBACK, а закрыл бы соединение, и пул недосчитался бы
// его ровно в тот момент, когда всё и так плохо. Закрытая транзакция —
// не ошибка: commit мог не дойти.
func rollback(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Rollback(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// setLocalStmt — таймауты одной командой на транзакцию.
//
// SET LOCAL, А НЕ DSN: параметр в DSN достался бы и миграциям, которые идут
// по тому же пулу и законно длинные. Значение не параметризуется ($1 в SET
// нельзя), поэтому собирается здесь из проверенных Config длительностей.
func setLocalStmt(cfg Config) string {
	return "SET LOCAL lock_timeout = '" + millis(cfg.LockTimeout) + "ms';" +
		" SET LOCAL statement_timeout = '" + millis(cfg.StatementTimeout) + "ms'"
}

// millis округляет ВВЕРХ: 500µs дали бы «0ms», а ноль в Postgres означает
// «таймаута нет» — тихое снятие защиты вместо очень короткой.
func millis(d time.Duration) string {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	return strconv.FormatInt(int64(ms), 10)
}

// backoff — экспонента 2^(attempt-1) от base с потолком 10×base, frac —
// полный джиттер [0,1). Джиттер полный, а не «половина плюс случайность»:
// две транзакции, подравшиеся за одни строки, должны разойтись во времени.
func backoff(base time.Duration, attempt int, frac float64) time.Duration {
	limit := 10 * base
	d := limit
	// Сдвиг на длинной серии повторов переполняется и даёт ноль или минус —
	// тогда пауза берётся потолком, а не пропадает.
	if grown := base << (attempt - 1); grown > 0 {
		d = min(grown, limit)
	}
	return time.Duration(frac * float64(d))
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
