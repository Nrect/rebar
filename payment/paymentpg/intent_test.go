package paymentpg_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// Два уникальных ограничения намерения различаются ПО ИМЕНИ, и исходы у них
// противоположные: занятый ключ идемпотентности — повтор, по которому домен
// отдаёт результат победителя; занятая ссылка — отказ, потому что второй платёж
// за тот же заказ означал бы два списания за одну покупку.
func TestStore_CreateIntent_IdempotencyRaceVsReferenceBusy(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	first := mustCreate(t, store, intent())

	sameKey := intent(func(in *payment.Intent) {
		in.PayerID = first.PayerID
		in.IdempotencyKey = first.IdempotencyKey
	})
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace)

	sameReference := intent(func(in *payment.Intent) { in.Reference = first.Reference })
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameReference), payment.ErrReferenceBusy)

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_intents`),
		"ни одна из отбитых попыток не оставила строки")

	// Ключ живёт в пространстве плательщика: чужой ключ не мешает и не выдаёт
	// чужую ссылку на оплату.
	otherPayer := intent(func(in *payment.Intent) { in.IdempotencyKey = first.IdempotencyKey })
	require.NoError(t, store.CreateIntent(t.Context(), otherPayer))

	// Терминальный статус освобождает ссылку: за тот же заказ можно заплатить
	// новой попыткой, а вот ключ остаётся занятым — отказ тоже результат.
	closeIntent(t, store, first)
	require.NoError(t, store.CreateIntent(t.Context(), sameReference))
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace)
}

// Гонка за один ключ: побеждает ровно один, остальные получают ErrIdempotencyRace
// и ни одной лишней строки. Арбитр — база, а не Go.
func TestStore_CreateIntent_Race(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	const workers = 4
	template := intent()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		results = make([]error, 0, workers)
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			candidate := intent(func(in *payment.Intent) {
				in.PayerID = template.PayerID
				in.IdempotencyKey = template.IdempotencyKey
				in.Reference = template.Reference
			})
			err := store.CreateIntent(t.Context(), candidate)
			mu.Lock()
			defer mu.Unlock()
			results = append(results, err)
			if err == nil {
				wins++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, wins, "ровно один победитель")
	for _, err := range results {
		if err != nil {
			require.ErrorIs(t, err, payment.ErrIdempotencyRace)
		}
	}
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_intents`))
}

// Состав и отпечаток возвращаются на каждом чтении и байт в байт: по составу
// собирают чек, а по отпечатку домен отличает законный повтор от чужой операции
// под тем же ключом.
func TestStore_CreateIntent_RoundTrip(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, paymentpg.Options{})
	in := intent(func(in *payment.Intent) {
		in.Items = []payment.OrderItem{
			{Position: 0, ProductID: "sku-1", Title: "Курс", AmountMinor: 50000, Quantity: 1},
			{Position: 1, ProductID: "sku-1", Title: "Курс со скидкой", AmountMinor: 29900, Quantity: 2},
		}
	})
	mustCreate(t, store, in)

	byKey, found, err := store.IntentByKey(t.Context(), in.PayerID, in.IdempotencyKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, in.Items, byKey.Items, "состав в порядке позиций")
	assert.Equal(t, in.ParamsFingerprint, byKey.ParamsFingerprint, "отпечаток байт в байт")
	assert.Equal(t, in.AmountMinor, byKey.AmountMinor)
	assert.Equal(t, in.Currency, byKey.Currency)
	assert.Equal(t, payment.StatusCreated, byKey.Status)
	assert.Equal(t, in.CreatedAt, byKey.CreatedAt)
	assert.Nil(t, byKey.SettledAt, "момент зачисления есть только у оплаченного")

	byID, found, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, byKey, byID, "оба чтения отдают одно и то же намерение")

	_, found, err = store.IntentByKey(t.Context(), uuid.New(), in.IdempotencyKey)
	require.NoError(t, err)
	assert.False(t, found, "ключ живёт в пространстве плательщика")

	_, found, err = store.IntentByID(t.Context(), uuid.New())
	require.NoError(t, err)
	assert.False(t, found, "отсутствие строки — не ошибка")
}

// Схема не принимает намерение без полного отпечатка: потерянная колонка тихо
// превратила бы чужую покупку под тем же ключом в законный повтор.
func TestStore_CreateIntent_RejectsBadFingerprint(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, paymentpg.Options{})
	tests := map[string]struct {
		fingerprint []byte
		want        string
	}{
		"колонку потеряли": {fingerprint: nil, want: "23502"},
		"отпечаток пустой": {fingerprint: []byte{}, want: "payment_intents_fingerprint_chk"},
		"отпечаток короче": {fingerprint: []byte{0x01, 0x02}, want: "payment_intents_fingerprint_chk"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := store.CreateIntent(t.Context(), intent(func(in *payment.Intent) {
				in.ParamsFingerprint = tt.fingerprint
			}))
			require.ErrorIs(t, err, payment.ErrUnavailable)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// closeIntent — довести намерение до терминального статуса без движения денег.
func closeIntent(t *testing.T, store *paymentpg.Store, in payment.Intent) {
	t.Helper()
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:   in.ID,
		ExpectFrom: expectFrom(payment.StatusCanceled),
		To:         payment.StatusCanceled,
		Now:        testNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
}
