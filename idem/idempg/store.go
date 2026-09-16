package idempg

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/postgres"
)

// Store — Do и idem.Pruner поверх таблицы idem_records (Migrations).
type Store struct {
	// pool и tx исключают друг друга: New даёт пул, WithTx — транзакцию
	// потребителя, в которой Do своей не открывает.
	pool *pgxpool.Pool
	tx   pgx.Tx
	cfg  idem.Config
	obs  idem.Observer
	now  func() time.Time
}

var _ idem.Pruner = (*Store)(nil)

// opFunc — операция под ключом: всё, что меняет состояние, в транзакции tx.
type opFunc = func(ctx context.Context, tx pgx.Tx) (idem.Response, error)

// New паникует на nil-пуле, nil-наблюдателе и негодном Config: ошибка сборки
// падает на старте. Config и наблюдатель те же, что у idemtest.NewMemStore;
// Config копируется.
func New(pool *pgxpool.Pool, cfg idem.Config, obs idem.Observer) *Store {
	if pool == nil {
		panic("idempg.New: nil pool")
	}
	if obs == nil {
		panic("idempg.New: observer must not be nil")
	}
	if err := cfg.Validate(); err != nil {
		panic("idempg.New: " + err.Error())
	}
	cfg.Operations = slices.Clone(cfg.Operations)
	return &Store{pool: pool, cfg: cfg, obs: obs, now: func() time.Time { return time.Now().UTC() }}
}

// WithTx — тот же адаптер в транзакции потребителя: ответ записывается вместе с
// его эффектом. ЛЮБАЯ ОШИБКА Do ПРЕРЫВАЕТ ЭТУ ТРАНЗАКЦИЮ, и её COMMIT не
// пройдёт (ADR-0012, решение 2.4).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("idempg.WithTx: nil tx")
	}
	return &Store{tx: tx, cfg: s.cfg, obs: s.obs, now: s.now}
}

// SetClock подменяет источник момента записи; только для тестов и до начала
// работы. nil — паника здесь, а не разыменование в чужом стеке.
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		panic("idempg.Store.SetClock: now must not be nil")
	}
	s.now = now
}

// db — исполнитель уборки и сверки схемы: транзакция потребителя, если она есть.
func (s *Store) db() postgres.Querier {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// Запросы Do. UPDATE нет: запись не переписывается (страж —
// TestAdapter_HasNoUpdate).
const (
	lockSQL   = `SELECT pg_try_advisory_xact_lock($1)`
	recordSQL = `SELECT fingerprint, status, content_type, location, body FROM idem_records
WHERE realm = $1 AND subject = $2 AND idem_key = $3`
	// ON CONFLICT DO NOTHING, А НЕ ПЕРЕХВАТ 23505: ошибка перевела бы транзакцию
	// потребителя в aborted (CORRECTNESS §4).
	insertSQL = `INSERT INTO idem_records
	(realm, subject, idem_key, operation, fingerprint, status, content_type, location, body, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT ON CONSTRAINT ux_idem_records_key DO NOTHING`
)

// Do — ответ на запрос под ключом (ADR-0012, решение 2): блокировка ключа,
// запись или op, ответ — одна транзакция; своя из пула, в WithTx — потребителя.
//
// Запрос, отвергнутый Config.CheckRequest, — его ошибка без наблюдателя. Ключ
// в работе — idem.InFlight() без ожидания. Ошибка op — как есть; ответ,
// отвергнутый Config.CheckResponse, — его ошибка; сбой базы —
// idem.ErrUnavailable. Записи ни в одном из этих случаев нет. Наблюдатель
// получает исход ровно раз.
func (s *Store) Do(ctx context.Context, req idem.Request,
	op func(context.Context, pgx.Tx) (idem.Response, error),
) (idem.Result, error) {
	if op == nil {
		panic("idempg.Store.Do: op must not be nil")
	}
	if s.tx != nil {
		return s.doInTx(ctx, req, op)
	}
	if err := s.cfg.CheckRequest(req); err != nil {
		return idem.Result{}, err
	}
	res, outcome, err := s.doInPool(ctx, req, op)
	s.obs.Outcome(ctx, req.Operation, outcome)
	return res, err
}

// doInPool — Do в своей транзакции: фиксируется только исполненная op.
func (s *Store) doInPool(ctx context.Context, req idem.Request, op opFunc) (idem.Result, idem.Outcome, error) {
	if ctx.Err() != nil {
		return idem.Result{}, idem.OutcomeError, storeError("begin", ctx.Err())
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return idem.Result{}, idem.OutcomeError, storeError("begin", err)
	}
	// Откат на отказе, на повторе и на панике op; после фиксации — пустой ход.
	defer rollback(ctx, tx)
	res, outcome, err := s.attempt(ctx, tx, req, op)
	if err != nil || outcome != idem.OutcomeExecuted {
		return res, outcome, err
	}
	if err = tx.Commit(ctx); err != nil {
		return idem.Result{}, idem.OutcomeError, storeError("commit", err)
	}
	return res, outcome, nil
}

// doInTx — Do в транзакции потребителя. Откатить её адаптер не вправе, поэтому
// после любой ошибки, отказа CheckRequest и паники op прерывает (abort).
func (s *Store) doInTx(ctx context.Context, req idem.Request, op opFunc) (idem.Result, error) {
	succeeded := false
	defer func() {
		if !succeeded {
			abort(ctx, s.tx)
		}
	}()
	if err := s.cfg.CheckRequest(req); err != nil {
		return idem.Result{}, err
	}
	res, outcome, err := s.attempt(ctx, s.tx, req, op)
	s.obs.Outcome(ctx, req.Operation, outcome)
	succeeded = err == nil
	return res, err
}

// attempt — шаги Do в транзакции tx: ключ, запись, op, проверка ответа, вставка.
func (s *Store) attempt(ctx context.Context, tx pgx.Tx, req idem.Request, op opFunc) (idem.Result, idem.Outcome, error) {
	var locked bool
	key := lockKey(req.Scope.Realm, req.Scope.Subject, req.Key.String())
	if err := tx.QueryRow(ctx, lockSQL, key).Scan(&locked); err != nil {
		return idem.Result{}, idem.OutcomeError, storeError("lock", err)
	}
	if !locked {
		return idem.Result{}, idem.OutcomeInFlight, idem.InFlight()
	}
	if res, outcome, found, err := replay(ctx, tx, req); found || err != nil {
		return res, outcome, err
	}
	resp, err := op(ctx, tx)
	if err != nil {
		return idem.Result{}, idem.OutcomeFailed, err
	}
	if refused := s.cfg.CheckResponse(resp); refused != nil {
		return idem.Result{}, refusedOutcome(refused), refused
	}
	inserted, err := s.insert(ctx, tx, req, resp)
	if err != nil {
		return idem.Result{}, idem.OutcomeError, err
	}
	if !inserted {
		return s.pastLock(ctx, tx, req)
	}
	return idem.Result{Response: resp}, idem.OutcomeExecuted, nil
}

// replay — запись под ключом запроса: тот же запрос получает её ответ, другой —
// idem.ErrKeyReused. found = false — записи нет.
func replay(ctx context.Context, tx pgx.Tx, req idem.Request) (res idem.Result, outcome idem.Outcome, found bool, err error) {
	var rec idem.Record
	err = tx.QueryRow(ctx, recordSQL, req.Scope.Realm, req.Scope.Subject, req.Key.String()).Scan(
		&rec.Fingerprint, &rec.Response.Status, &rec.Response.ContentType, &rec.Response.Location, &rec.Response.Body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return idem.Result{}, "", false, nil
	case err != nil:
		return idem.Result{}, idem.OutcomeError, false, storeError("read record", err)
	}
	if res, err = idem.Replay(req, rec); err != nil {
		return idem.Result{}, idem.OutcomeReused, true, err
	}
	return res, idem.OutcomeReplayed, true, nil
}

// insert — запись ответа; false — под ключом уже есть запись.
func (s *Store) insert(ctx context.Context, tx pgx.Tx, req idem.Request, resp idem.Response) (bool, error) {
	body := resp.Body
	if body == nil {
		body = []byte{} // nil pgx отправил бы NULL
	}
	tag, err := tx.Exec(ctx, insertSQL, req.Scope.Realm, req.Scope.Subject, req.Key.String(), string(req.Operation),
		req.Fingerprint(), resp.Status, resp.ContentType, resp.Location, body, moment(s.now()))
	if err != nil {
		return false, storeError("insert record", err)
	}
	return tag.RowsAffected() == 1, nil
}

// pastLock — вставка упёрлась в запись, которой под взятой блокировкой быть не
// могло: её записали мимо блокировки (раннер без неё, другая формула ключа на
// выкате, ручной SQL). Решение — по перечитанной записи.
//
// В WithTx УСПЕХА НЕТ: эффект op уже в транзакции потребителя, и его фиксация
// исполнила бы запрос второй раз. Отказ прерывает транзакцию, повтор получит
// запись. В пуле эффект op уходит откатом.
func (s *Store) pastLock(ctx context.Context, tx pgx.Tx, req idem.Request) (idem.Result, idem.Outcome, error) {
	res, outcome, found, err := replay(ctx, tx, req)
	switch {
	case err != nil:
		return idem.Result{}, outcome, err
	case !found:
		return idem.Result{}, idem.OutcomeError, errVanished
	case s.tx != nil:
		return idem.Result{}, idem.OutcomeError, errPastLock
	}
	return res, outcome, nil
}

// refusedOutcome — исход отказа Config.CheckResponse.
func refusedOutcome(err error) idem.Outcome {
	if errors.Is(err, idem.ErrResponseTooLarge) {
		return idem.OutcomeTooLarge
	}
	return idem.OutcomeNotRecordable
}

// abortSQL — ЗАВЕДОМО ПАДАЮЩИЙ ЗАПРОС (решение 2.4, уточнение арбитра 1): после
// отказа транзакция потребителя уходит в aborted, и его COMMIT не пройдёт.
// Иначе проигнорированная ошибка закоммитила бы эффект без записи, и повтор
// исполнил бы его второй раз. 25P02 postgres.IsRetryable не повторяет; текст
// называет модуль, чтобы в логе базы и в трассировке не искали ошибку SQL.
const abortSQL = `DO $$ BEGIN RAISE EXCEPTION 'idempg: транзакция прервана после отказа' USING ERRCODE = '25P02'; END $$`

// abort — мимо отмены ctx: по отменённому ctx pgx запрос не отправил бы. Ошибка
// запроса и есть цель, как и отказ уже прерванной транзакции.
func abort(ctx context.Context, tx pgx.Tx) {
	_, _ = tx.Exec(context.WithoutCancel(ctx), abortSQL)
}

// rollback — мимо отмены ctx: по отменённому ctx pgx не отправил бы ROLLBACK, а
// закрыл бы соединение.
func rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(context.WithoutCancel(ctx))
}

// purgeSQL — самые старые первыми; равные моменты — по ключу побайтно, как у
// двойника: иначе порядок решала бы локаль базы.
const purgeSQL = `DELETE FROM idem_records WHERE (realm, subject, idem_key) IN (
	SELECT realm, subject, idem_key FROM idem_records WHERE created_at < $1
	ORDER BY created_at, realm COLLATE "C", subject COLLATE "C", idem_key COLLATE "C"
	LIMIT $2
)`

// Purge удаляет записи с моментом записи раньше before, самые старые первыми, не
// больше limit (контракт idem.Pruner). Непозитивный limit — ноль без ошибки и
// без базы.
func (s *Store) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if ctx.Err() != nil {
		return 0, storeError("purge", ctx.Err())
	}
	tag, err := s.db().Exec(ctx, purgeSQL, moment(before), limit)
	if err != nil {
		return 0, storeError("purge", err)
	}
	return int(tag.RowsAffected()), nil // не больше limit по построению запроса
}

// moment — момент так, как его хранит timestamptz: UTC и микросекунды.
func moment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
