package paymentpg

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/postgres"
)

// ON CONFLICT ON CONSTRAINT, А НЕ ПЕРЕХВАТ 23505: законный повтор по ключу
// идемпотентности — самый частый исход этой вставки (двойной клик, ретрай
// клиента), а ошибка Postgres перевела бы транзакцию потребителя в aborted
// целиком и уронила бы бизнес-факт, вместе с которым намерение и создаётся.
// Арбитр назван по имени, поэтому конфликт живой ссылки остаётся ошибкой и
// разбирается отдельным исходом (ErrReferenceBusy), а не «дублем».
const insertIntentSQL = `INSERT INTO payment_intents (` + intentColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT ON CONSTRAINT ` + uxIntentsKey + ` DO NOTHING`

// Состав уезжает одним запросом массивами: строка на позицию превратила бы
// вставку намерения в N круговых обходов внутри транзакции потребителя.
const insertItemsSQL = `INSERT INTO payment_intent_items
	(intent_id, position, product_id, title, amount_minor, quantity)
SELECT $1, * FROM unnest($2::int[], $3::text[], $4::text[], $5::bigint[], $6::int[])`

const selectIntentByKeySQL = `SELECT ` + intentColumns + ` FROM payment_intents
WHERE payer_id = $1 AND idempotency_key = $2`

const selectIntentByIDSQL = `SELECT ` + intentColumns + ` FROM payment_intents WHERE id = $1`

const selectItemsSQL = `SELECT intent_id, position, product_id, title, amount_minor, quantity
FROM payment_intent_items WHERE intent_id = ANY($1) ORDER BY intent_id, position`

// CreateIntent вставляет намерение вместе с составом одной транзакцией:
// намерение без состава — это списание, к которому нечего приложить чеком.
func (s *Store) CreateIntent(ctx context.Context, in payment.Intent) error {
	return s.inTx(ctx, "create intent", func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, insertIntentSQL,
			in.ID, in.PayerID, in.Reference, in.AmountMinor, in.Currency,
			string(in.Provider), string(in.Method), in.AutoCapture, in.ProviderPaymentID,
			string(in.Confirmation.Type), in.Confirmation.URL, in.Confirmation.QRPayload,
			string(in.Status), in.IdempotencyKey, in.ParamsFingerprint,
			in.CreatedAt, in.UpdatedAt, in.ExpiresAt, settledAt(in))
		switch {
		case postgres.IsUniqueViolation(err, uxIntentsLiveReference):
			return payment.ErrReferenceBusy
		case postgres.IsUniqueViolation(err, uxIntentsKey):
			// Сюда попадёт гонка, в которой победитель закоммитился между нашим
			// ON CONFLICT и вставкой: арбитр её уже не гасит.
			return payment.ErrIdempotencyRace
		case err != nil:
			return storeError("create intent", err)
		case tag.RowsAffected() == 0:
			return payment.ErrIdempotencyRace
		}
		return s.insertItems(ctx, tx, in)
	})
}

func (s *Store) insertItems(ctx context.Context, tx pgx.Tx, in payment.Intent) error {
	positions := make([]int32, 0, len(in.Items))
	products := make([]string, 0, len(in.Items))
	titles := make([]string, 0, len(in.Items))
	amounts := make([]int64, 0, len(in.Items))
	quantities := make([]int32, 0, len(in.Items))
	for _, item := range in.Items {
		positions = append(positions, int32(item.Position)) //nolint:gosec // позиция ≤ Config.MaxItems
		products = append(products, item.ProductID)
		titles = append(titles, item.Title)
		amounts = append(amounts, item.AmountMinor)
		quantities = append(quantities, int32(item.Quantity)) //nolint:gosec // количество проверено CheckItems
	}
	_, err := tx.Exec(ctx, insertItemsSQL, in.ID, positions, products, titles, amounts, quantities)
	return storeError("create intent: items", err)
}

// settledAt — момент зачисления как его хранит колонка: NULL у всего, кроме
// succeeded (payment_intents_settled_chk).
func settledAt(in payment.Intent) any {
	if in.Status != payment.StatusSucceeded || in.SettledAt == nil {
		return nil
	}
	return *in.SettledAt
}

// IntentByKey — проба идемпотентности в пространстве (payer_id, key).
// Ключ приходит уже нормализованным; адаптер его не трогает.
func (s *Store) IntentByKey(ctx context.Context, payerID uuid.UUID, key string,
) (payment.Intent, bool, error) {
	return s.readIntent(ctx, "intent by key", selectIntentByKeySQL, payerID, key)
}

// IntentByID — чтение для вебхука, сверки и админки.
func (s *Store) IntentByID(ctx context.Context, id uuid.UUID) (payment.Intent, bool, error) {
	return s.readIntent(ctx, "intent by id", selectIntentByIDSQL, id)
}

func (s *Store) readIntent(ctx context.Context, op, sql string, args ...any,
) (payment.Intent, bool, error) {
	in, err := scanIntent(s.db().QueryRow(ctx, sql, args...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return payment.Intent{}, false, nil
	case errors.Is(err, payment.ErrBadStatus):
		return payment.Intent{}, false, err
	case err != nil:
		return payment.Intent{}, false, storeError(op, err)
	}
	if in.Items, err = s.itemsOf(ctx, s.db(), in.ID); err != nil {
		return payment.Intent{}, false, err
	}
	return in, true, nil
}

// itemsOf — состав одного намерения. Возвращается на КАЖДОМ чтении: состав,
// заполненный «иногда», однажды окажется пустым там, где по нему собирают чек.
func (s *Store) itemsOf(ctx context.Context, q querier, id uuid.UUID) ([]payment.OrderItem, error) {
	byIntent, err := loadItems(ctx, q, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	return byIntent[id], nil
}

// loadItems — состав пачки намерений одним запросом.
func loadItems(ctx context.Context, q querier, ids []uuid.UUID) (map[uuid.UUID][]payment.OrderItem, error) {
	rows, err := q.Query(ctx, selectItemsSQL, ids)
	if err != nil {
		return nil, storeError("intent items", err)
	}
	defer rows.Close()

	byIntent := make(map[uuid.UUID][]payment.OrderItem, len(ids))
	for rows.Next() {
		var (
			id   uuid.UUID
			item payment.OrderItem
		)
		if err = rows.Scan(&id, &item.Position, &item.ProductID, &item.Title,
			&item.AmountMinor, &item.Quantity); err != nil {
			return nil, storeError("intent items", err)
		}
		byIntent[id] = append(byIntent[id], item)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("intent items", err)
	}
	return byIntent, nil
}
