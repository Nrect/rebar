package shoppg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/postgres"
)

const (
	insertOrderSQL = `INSERT INTO shop_orders
(id, subject_id, product_code, amount_minor, currency, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`

	selectOrderSQL = `SELECT id, subject_id, product_code, amount_minor, currency, paid_at
FROM shop_orders WHERE id = $1`

	// Блокировка заказа — ПЕРВЫЙ шаг хука и первая из его собственных строк:
	// порядок обхода продолжает порядок paymentpg (намерение → события →
	// книга → хук). Пометка идемпотентна: paid_at IS NULL в WHERE.
	lockOrderSQL = `SELECT id, subject_id, product_code, amount_minor, currency, paid_at
FROM shop_orders WHERE id = $1 FOR UPDATE`
	markOrderPaidSQL = `UPDATE shop_orders SET paid_at = $2 WHERE id = $1 AND paid_at IS NULL`
)

// Order — заказ потребителя. Reference в намерении оплаты — это его id.
type Order struct {
	ID          uuid.UUID
	SubjectID   uuid.UUID
	ProductCode string
	AmountMinor int64
	Currency    string
	PaidAt      *time.Time
}

// Paid — оплачен ли заказ.
func (o Order) Paid() bool { return o.PaidAt != nil }

// Orders — таблица заказов.
type Orders struct {
	db postgres.Querier
}

// NewOrders — адаптер на пуле.
func NewOrders(db *DB) *Orders { return &Orders{db: db.Pool} }

// WithTx — тот же адаптер в транзакции вызывающего.
func (s *Orders) WithTx(tx pgx.Tx) *Orders {
	if tx == nil {
		panic("shoppg.Orders.WithTx: nil tx")
	}
	return &Orders{db: tx}
}

// Create заводит заказ.
func (s *Orders) Create(ctx context.Context, o Order, at time.Time) error {
	_, err := s.db.Exec(ctx, insertOrderSQL, o.ID, o.SubjectID, o.ProductCode,
		o.AmountMinor, o.Currency, utc(at))
	return storeError("создание заказа", err)
}

// ByID читает заказ; ok == false — строки нет.
func (s *Orders) ByID(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	return scanOrder(s.db.QueryRow(ctx, selectOrderSQL, id))
}

// Lock читает заказ под FOR UPDATE. Зовётся из хука зачисления первым.
func (s *Orders) Lock(ctx context.Context, id uuid.UUID) (Order, bool, error) {
	return scanOrder(s.db.QueryRow(ctx, lockOrderSQL, id))
}

// MarkPaid помечает заказ оплаченным. Повтор ничего не меняет: строка уже
// оплачена, и второй вебхук не обязан быть ошибкой.
func (s *Orders) MarkPaid(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.db.Exec(ctx, markOrderPaidSQL, id, utc(at))
	return storeError("пометка заказа оплаченным", err)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrder(row rowScanner) (Order, bool, error) {
	var o Order
	var paid *time.Time
	err := row.Scan(&o.ID, &o.SubjectID, &o.ProductCode, &o.AmountMinor, &o.Currency, &paid)
	switch {
	case noRows(err):
		return Order{}, false, nil
	case err != nil:
		return Order{}, false, storeError("чтение заказа", err)
	}
	if paid != nil {
		moment := paid.UTC()
		o.PaidAt = &moment
	}
	return o, true, nil
}
