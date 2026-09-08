package paymentpg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/payment"
)

// Settler — хук потребителя, объявленный контрактом порта (payment/ports.go,
// «Хук потребителя»). Типы pgx здесь законны: это адаптер, и транзакция тут и
// так видна, — а порт ядра остался бы с чужим типом в сигнатуре.
//
// Зовётся ПОСЛЕ записи книги и ДО commit той же транзакцией. Ошибка хука
// откатывает всё, включая строку дедупа события: иначе повтор вебхука увидел бы
// дубль, не применил бы ничего, а провайдер получил бы 200 на неучтённую оплату.
//
// Обе половины обязательны намеренно. Возврат — тоже движение денег, и
// потребитель, забывший про него, узнаёт об этом от клиента, а не от
// компилятора.
type Settler interface {
	OnSettled(ctx context.Context, tx pgx.Tx, in payment.Intent, entry payment.LedgerEntry) error
	OnRefunded(ctx context.Context, tx pgx.Tx, in payment.Intent, entry payment.LedgerEntry) error
}

// Options — что адаптер знает сверх пула.
type Options struct {
	// Settler — эффекты потребителя в транзакции зачисления и возврата.
	// nil законен и означает «эффектов нет».
	Settler Settler
}

// Store — payment.Store поверх таблиц payment_* (schema.sql).
type Store struct {
	// pool и tx исключают друг друга: New даёт пул, WithTx — транзакцию
	// потребителя. Второй режим не открывает своей транзакции, иначе
	// «в одной транзакции с бизнес-фактом» было бы пожеланием.
	pool *pgxpool.Pool
	tx   pgx.Tx
	opts Options
}

var _ payment.Store = (*Store)(nil)

// querier — общий знаменатель *pgxpool.Pool и pgx.Tx: ровно то, чем
// пользуются запросы адаптера.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New паникует на nil-пуле: ошибка сборки приложения падает на старте, а не на
// первом платеже (как payment.NewService).
func New(pool *pgxpool.Pool, opts Options) *Store {
	if pool == nil {
		panic("paymentpg.New: nil pool")
	}
	return &Store{pool: pool, opts: opts}
}

// WithTx — тот же адаптер в транзакции потребителя: намерение, статус и книга
// ложатся вместе с его бизнес-фактом (ADR-0004).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("paymentpg.WithTx: nil tx")
	}
	return &Store{tx: tx, opts: s.opts}
}

// db — исполнитель для чтения: транзакция потребителя, если она есть.
func (s *Store) db() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// inTx проводит fn через ОДНУ транзакцию. В режиме WithTx это транзакция
// потребителя целиком: своей вложенной мы не открываем — сберпоинт пережил бы
// ошибку хука, а по контракту она обязана откатить и бизнес-факт тоже.
//
// Ошибки fn проходят наружу как есть: домен ветвится по payment.Err*, и
// заворачивать их в ErrUnavailable здесь значило бы сделать отказ сбоем.
func (s *Store) inTx(ctx context.Context, op string, fn func(context.Context, pgx.Tx) error) error {
	if s.tx != nil {
		return fn(ctx, s.tx)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storeError(op, err)
	}
	// Откат идёт мимо отмены ctx: по отменённому ctx pgx не отправил бы
	// ROLLBACK, а закрыл бы соединение — пул недосчитался бы его ровно тогда,
	// когда всё и так плохо. Панику это тоже накрывает.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if txErr := fn(ctx, tx); txErr != nil {
		return txErr
	}
	return storeError(op, tx.Commit(ctx))
}

// intentColumns — порядок колонок для scanIntent; менять только вместе с ним.
const intentColumns = `id, payer_id, reference, amount_minor, currency, provider, method,
	auto_capture, provider_payment_id, confirmation_type, confirmation_url, confirmation_qr,
	status, idempotency_key, params_fingerprint, created_at, updated_at, expires_at, settled_at`

// scanner — общее у pgx.Row и pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanIntent — строка намерения без состава: состав докладывает attachItems.
//
// Статус разбирается payment.ParseStatus, а не приводится строкой: незнакомое
// значение в денежной строке — это домысел про оплату (payment/status.go).
func scanIntent(s scanner) (payment.Intent, error) {
	var (
		in       payment.Intent
		status   string
		provider string
		method   string
		confType string
		settled  *time.Time
	)
	err := s.Scan(&in.ID, &in.PayerID, &in.Reference, &in.AmountMinor, &in.Currency,
		&provider, &method, &in.AutoCapture, &in.ProviderPaymentID,
		&confType, &in.Confirmation.URL, &in.Confirmation.QRPayload,
		&status, &in.IdempotencyKey, &in.ParamsFingerprint,
		&in.CreatedAt, &in.UpdatedAt, &in.ExpiresAt, &settled)
	if err != nil {
		return payment.Intent{}, err
	}
	if in.Status, err = payment.ParseStatus(status); err != nil {
		return payment.Intent{}, err
	}
	in.Provider = payment.ProviderName(provider)
	in.Method = payment.Method(method)
	in.Confirmation.Type = payment.ConfirmationType(confType)
	// pgx отдаёт timestamptz в зоне соединения, а порт говорит о моментах.
	in.CreatedAt = in.CreatedAt.UTC()
	in.UpdatedAt = in.UpdatedAt.UTC()
	in.ExpiresAt = in.ExpiresAt.UTC()
	in.SettledAt = utcPtr(settled)
	return in, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// nilIfEmpty — uuid.Nil в NULL: орфан у события и автоматическая запись книги
// хранятся как отсутствие ссылки, а не как нулевой uuid.
func nilIfEmpty(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
