package paymentpg_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// Очередь сверки отдаётся в порядке (created_at, id) и продвигается курсором:
// без курсора пачка намерений в голове очереди, о которых провайдер отвечает
// ошибкой, навсегда заслонила бы хвост — а в хвосте лежит тот, кто заплатил
// только что и чей вебхук потерялся.
func TestStore_StalePending_OrderAndCursor(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, paymentpg.Options{})
	base := testNow().Add(-time.Hour)
	open := make([]payment.Intent, 0, 5)
	for i := range 5 {
		in := mustCreate(t, store, intent(func(in *payment.Intent) {
			in.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			in.UpdatedAt = in.CreatedAt
			in.ExpiresAt = in.CreatedAt.Add(time.Hour)
		}))
		open = append(open, in)
	}
	// Терминальное намерение очередь не кормит: с ним провайдеру говорить не о чем.
	closed := mustCreate(t, store, intent(func(in *payment.Intent) {
		in.CreatedAt = base
		in.UpdatedAt = base
		in.ExpiresAt = base.Add(time.Hour)
	}))
	closeIntent(t, store, closed)
	// Свежее намерение старше порога не считается зависшим.
	fresh := mustCreate(t, store, intent())

	olderThan := testNow().Add(-time.Minute)
	seen := make([]uuid.UUID, 0, len(open))
	cursor := payment.IntentCursor{}
	for range 3 {
		batch, err := store.StalePending(t.Context(), olderThan, cursor, 2)
		require.NoError(t, err)
		for _, in := range batch {
			seen = append(seen, in.ID)
			assert.NotEmpty(t, in.Items, "состав едет вместе с намерением")
			cursor = payment.IntentCursor{CreatedAt: in.CreatedAt, ID: in.ID}
		}
	}

	want := make([]uuid.UUID, 0, len(open))
	for _, in := range open {
		want = append(want, in.ID)
	}
	assert.Equal(t, want, seen, "порядок очереди и продвижение курсора")
	assert.NotContains(t, seen, closed.ID)
	assert.NotContains(t, seen, fresh.ID)

	// Пачка «на ноль строк» тихо остановила бы сверку: это ошибка программиста.
	_, err := store.StalePending(t.Context(), olderThan, payment.IntentCursor{}, 0)
	require.ErrorIs(t, err, payment.ErrBadTransition)
}

// Gauge зависших считается ВСЕМИ строками, а не пачкой: LIMIT превратил бы
// «зависших 5000» в «зависших 2» ровно тогда, когда число и есть содержание
// тревоги.
func TestStore_CountStuckPending(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, paymentpg.Options{})
	base := testNow().Add(-time.Hour)
	for i := range 5 {
		mustCreate(t, store, intent(func(in *payment.Intent) {
			in.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			in.UpdatedAt = in.CreatedAt
			in.ExpiresAt = in.CreatedAt.Add(2 * time.Hour)
		}))
	}
	mustCreate(t, store, intent()) // свежее: не зависшее

	n, err := store.CountStuckPending(t.Context(), testNow().Add(-time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(5), n)

	batch, err := store.StalePending(t.Context(), testNow().Add(-time.Minute), payment.IntentCursor{}, 2)
	require.NoError(t, err)
	assert.Len(t, batch, 2, "у пачки потолок есть, у gauge — нет")
}

// Ноль расхождений — утверждение, которое должно проверяться, а не
// подразумеваться. Все три рода считаются по книге, а не по колонке «оплачено».
func TestStore_Drift(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	healthy := mustCreate(t, store, intent())
	settleIntent(t, store, healthy)

	records, err := store.Drift(t.Context(), testNow(), 10)
	require.NoError(t, err)
	assert.Empty(t, records, "здоровая книга расхождений не даёт")

	// Оплачено без записи зачисления: статус проехал, книга нет.
	noCapture := mustCreate(t, store, intent())
	toPending(t, store, noCapture)
	_, err = pool.Exec(t.Context(),
		`UPDATE payment_intents SET status = 'succeeded', settled_at = $2 WHERE id = $1`,
		noCapture.ID, testNow())
	require.NoError(t, err)

	// Деньги в книге на неоплаченном намерении.
	notSucceeded := mustCreate(t, store, intent())
	toPending(t, store, notSucceeded)
	_, err = pool.Exec(t.Context(), `INSERT INTO payment_ledger (id, intent_id, kind, amount_minor,
		currency, provider_event_id, reverses_entry_id, idempotency_key, actor_id, created_at)
		VALUES ($1, $2, 'capture', $3, $4, '', NULL, 'capture:bypass', NULL, $5)`,
		uuid.New(), notSucceeded.ID, notSucceeded.AmountMinor, notSucceeded.Currency, testNow())
	require.NoError(t, err)

	records, err = store.Drift(t.Context(), testNow(), 10)
	require.NoError(t, err)
	kinds := map[uuid.UUID]payment.DriftKind{}
	for _, rec := range records {
		kinds[rec.IntentID] = rec.Kind
		assert.NotEmpty(t, rec.Reference, "по ссылке расхождение и разбирают")
	}
	assert.Equal(t, payment.DriftSucceededNoCapture, kinds[noCapture.ID])
	assert.Equal(t, payment.DriftCaptureNotSucceeded, kinds[notSucceeded.ID])
	assert.NotContains(t, kinds, healthy.ID)

	_, err = store.Drift(t.Context(), testNow(), 0)
	require.ErrorIs(t, err, payment.ErrBadTransition)
}
