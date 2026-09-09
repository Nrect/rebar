package paymentpg

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// Очередь сверки: порядок (created_at, id) — тот же ключ, каким устроен курсор.
// Порядок обязателен, а не желателен: пачка в другом порядке превратила бы
// продвижение курсора в тихий пропуск намерений, а пропущенное намерение — это
// человек, который заплатил и ждёт.
const selectStaleSQL = `SELECT ` + intentColumns + ` FROM payment_intents
WHERE status = ANY($1) AND created_at < $2 AND (created_at, id) > ($3, $4)
ORDER BY created_at, id
LIMIT $5`

const countStuckSQL = `SELECT count(*) FROM payment_intents
WHERE status = ANY($1) AND created_at < $2`

// Расхождения книг. Суммы считаются по книге, а не по колонке «оплачено»:
// материализованная сумма разъезжается с книгой молча.
const selectDriftSQL = `WITH sums AS (
	SELECT intent_id,
		COALESCE(SUM(amount_minor) FILTER (WHERE kind = 'capture'), 0) AS captured,
		COALESCE(SUM(amount_minor) FILTER (WHERE kind = 'refund'), 0) AS refunded
	FROM payment_ledger GROUP BY intent_id)
SELECT i.id, i.payer_id, i.reference, '` + string(payment.DriftSucceededNoCapture) + `' AS kind
	FROM payment_intents i LEFT JOIN sums s ON s.intent_id = i.id
	WHERE i.status = 'succeeded' AND COALESCE(s.captured, 0) = 0
UNION ALL
SELECT i.id, i.payer_id, i.reference, '` + string(payment.DriftCaptureNotSucceeded) + `'
	FROM payment_intents i JOIN sums s ON s.intent_id = i.id
	WHERE s.captured > 0 AND i.status <> 'succeeded'
UNION ALL
SELECT i.id, i.payer_id, i.reference, '` + string(payment.DriftRefundOverCapture) + `'
	FROM payment_intents i JOIN sums s ON s.intent_id = i.id
	WHERE s.refunded > s.captured
ORDER BY 1, 4
LIMIT $1`

// StalePending — незакрытые намерения старше olderThan, стоящие в очереди
// строго после курсора.
func (s *Store) StalePending(ctx context.Context, olderThan time.Time,
	after payment.IntentCursor, limit int,
) ([]payment.Intent, error) {
	if limit <= 0 {
		// Ошибка программиста: пачка «на ноль строк» тихо остановила бы сверку,
		// а отрицательный LIMIT Postgres не примет вовсе.
		return nil, fmt.Errorf("%w: stale pending limit must be positive, got %d",
			payment.ErrBadTransition, limit)
	}
	rows, err := s.db().Query(ctx, selectStaleSQL, openStatuses(), olderThan,
		after.CreatedAt, after.ID, limit)
	if err != nil {
		return nil, storeError("stale pending", err)
	}
	defer rows.Close()

	intents := make([]payment.Intent, 0, limit)
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		in, scanErr := scanIntent(rows)
		if scanErr != nil {
			return nil, storeError("stale pending", scanErr)
		}
		intents = append(intents, in)
		ids = append(ids, in.ID)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("stale pending", err)
	}

	byIntent, err := loadItems(ctx, s.db(), ids)
	if err != nil {
		return nil, err
	}
	for i := range intents {
		intents[i].Items = byIntent[intents[i].ID]
	}
	return intents, nil
}

// CountStuckPending — сколько ВСЕГО незакрытых намерений старше olderThan.
// Потолка нет намеренно: LIMIT превратил бы «зависших 5000» в «зависших 200»
// ровно тогда, когда число и есть содержание тревоги.
func (s *Store) CountStuckPending(ctx context.Context, olderThan time.Time) (int64, error) {
	var n int64
	if err := s.db().QueryRow(ctx, countStuckSQL, openStatuses(), olderThan).Scan(&n); err != nil {
		return 0, storeError("count stuck pending", err)
	}
	return n, nil
}

// Drift — расхождения книг.
//
// Момент не участвует в запросе: расхождение — утверждение о книгах, а не о
// времени. Зачисление, идущее прямо сейчас, в выборку и так не попадёт — его
// транзакция не закоммичена, и READ COMMITTED её не видит. Параметр остаётся в
// порту: другому хранилищу окно может понадобиться.
func (s *Store) Drift(ctx context.Context, _ time.Time, limit int) ([]payment.DriftRecord, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: drift limit must be positive, got %d",
			payment.ErrBadTransition, limit)
	}
	rows, err := s.db().Query(ctx, selectDriftSQL, limit)
	if err != nil {
		return nil, storeError("drift", err)
	}
	defer rows.Close()

	records := make([]payment.DriftRecord, 0, limit)
	for rows.Next() {
		var (
			rec  payment.DriftRecord
			kind string
		)
		if err = rows.Scan(&rec.IntentID, &rec.PayerID, &rec.Reference, &kind); err != nil {
			return nil, storeError("drift", err)
		}
		rec.Kind = payment.DriftKind(kind)
		records = append(records, rec)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("drift", err)
	}
	return records, nil
}

// openStatuses — незакрытые статусы для выборок очереди. Считаются по таблице
// переходов ядра, а не вторым списком: список, разъехавшийся с ядром, увёл бы
// из очереди сверки целый статус — то есть спрятал бы зависшие деньги.
// Совпадение с частичным индексом схемы держит TestOpenStatuses_MatchSchemaFile.
func openStatuses() []string {
	out := make([]string, 0, len(payment.AllStatuses))
	for _, st := range payment.AllStatuses {
		if st.IsOpen() {
			out = append(out, string(st))
		}
	}
	return out
}
