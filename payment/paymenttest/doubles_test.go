package paymenttest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func intent(reference, key string) payment.Intent {
	return payment.Intent{
		ID:          uuid.New(),
		PayerID:     uuid.MustParse("55555555-5555-5555-5555-555555555555"),
		Reference:   reference,
		AmountMinor: 1000,
		Currency:    "RUB",
		Items: []payment.OrderItem{
			{Position: 0, ProductID: "a", AmountMinor: 1000, Quantity: 1},
		},
		Provider:          "memprov",
		Status:            payment.StatusCreated,
		IdempotencyKey:    key,
		ParamsFingerprint: []byte("fp"),
		CreatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}
}

// Двойник держит ОБА ограничения схемы и различает их: уникальность ключа — это
// повтор, занятая ссылка — отказ.
func TestMemStore_CreateIntent_Constraints(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	first := intent("order:1", "buy-1")
	require.NoError(t, store.CreateIntent(t.Context(), first))

	sameKey := intent("order:2", "buy-1")
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace)

	sameReference := intent("order:1", "buy-2")
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameReference), payment.ErrReferenceBusy)
}

// Терминальный статус освобождает ссылку: за тот же заказ можно заплатить новой
// попыткой.
func TestMemStore_TerminalStatusFreesReference(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	first := intent("order:1", "buy-1")
	require.NoError(t, store.CreateIntent(t.Context(), first))

	_, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:   first.ID,
		ExpectFrom: []payment.Status{payment.StatusCreated},
		To:         payment.StatusCanceled,
		Now:        now,
	})
	require.NoError(t, err)

	require.NoError(t, store.CreateIntent(t.Context(), intent("order:1", "buy-2")))
}

// Состав возвращается на КАЖДОМ чтении и копией: правка полученного не должна
// доезжать до «базы».
func TestMemStore_ReadsReturnItemsSnapshot(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	in := intent("order:1", "buy-1")
	require.NoError(t, store.CreateIntent(t.Context(), in))

	got, found, err := store.IntentByKey(t.Context(), in.PayerID, "buy-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, got.Items, 1)
	got.Items[0].AmountMinor = 1

	again, _, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), again.Items[0].AmountMinor)
	assert.Equal(t, []byte("fp"), again.ParamsFingerprint, "отпечаток возвращается байт в байт")
}

// Потолок возвратов держит книга, а не домен: частичные складываются, а сверх
// зачисления не проходит.
func TestMemStore_ApplyRefund_LedgerCap(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	in := intent("order:1", "buy-1")
	in.Status = payment.StatusSucceeded
	store.Seed(in)
	capture := payment.LedgerEntry{
		ID: uuid.New(), IntentID: in.ID, Kind: payment.LedgerCapture,
		AmountMinor: 1000, Currency: "RUB", IdempotencyKey: "capture:1",
	}
	store.SeedEntry(capture)

	refund := func(amount int64, key string) payment.ApplyRefundResult {
		t.Helper()
		captureID := capture.ID
		res, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
			IntentID: in.ID, CaptureEntryID: captureID, Now: now,
			Refund: payment.LedgerEntry{
				ID: uuid.New(), IntentID: in.ID, Kind: payment.LedgerRefund,
				AmountMinor: amount, Currency: "RUB",
				ReversesEntryID: &captureID, IdempotencyKey: key,
			},
		})
		require.NoError(t, err)
		return res
	}

	assert.Equal(t, payment.OutcomeApplied, refund(600, "r1").Outcome)
	assert.Equal(t, payment.OutcomeApplied, refund(400, "r2").Outcome)
	assert.Equal(t, payment.OutcomeRefundTooLarge, refund(1, "r3").Outcome)
	// Тот же ключ — та же строка, а не вторая.
	assert.Equal(t, payment.OutcomeDuplicateEvent, refund(600, "r1").Outcome)
	assert.Len(t, store.EntriesOf(in.ID, payment.LedgerRefund), 2)
}

// Форму запроса двойник требует ту же, что адаптер: запись на чужое намерение
// уехала бы мимо блокировки, и потолок считался бы по чужой книге.
func TestMemStore_ApplyRefund_RefusesBrokenShape(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	in := intent("order:1", "buy-1")
	store.Seed(in)
	captureID := uuid.New()

	_, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: captureID, Now: now,
		Refund: payment.LedgerEntry{
			ID: uuid.New(), IntentID: uuid.New(), Kind: payment.LedgerRefund,
			AmountMinor: 100, Currency: "RUB", ReversesEntryID: &captureID,
		},
	})

	require.ErrorIs(t, err, payment.ErrBadTransition)
}

// Пачка очереди сверки упорядочена ДО обрезки: иначе двойник отдавал бы
// случайное подмножество в правильном порядке и скрывал голодание хвоста.
func TestMemStore_StalePending_OrdersBeforeLimiting(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	for i := range 5 {
		in := intent(fmt.Sprintf("order:%d", i), fmt.Sprintf("buy-%d", i))
		in.Status = payment.StatusPending
		in.CreatedAt = now.Add(time.Duration(i) * time.Second)
		store.Seed(in)
	}

	batch, err := store.StalePending(t.Context(), now.Add(time.Hour), payment.IntentCursor{}, 2)

	require.NoError(t, err)
	require.Len(t, batch, 2)
	assert.True(t, batch[0].CreatedAt.Before(batch[1].CreatedAt))
	assert.Equal(t, now, batch[0].CreatedAt, "голова очереди — самое старое намерение")

	count, err := store.CountStuckPending(t.Context(), now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(5), count, "счётчик не упирается в размер пачки")
}

// Идемпотентность провайдера смоделирована по-настоящему: повтор с тем же
// ключом возвращает ТОТ ЖЕ платёж, а не второй.
func TestMemProvider_CreatePaymentIsIdempotent(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	req := payment.CreatePaymentRequest{
		IntentID: uuid.New(), Reference: "order:1", AmountMinor: 1000,
		Currency: "RUB", IdempotencyKey: "shop:intent:1",
	}

	first, err := prov.CreatePayment(t.Context(), req)
	require.NoError(t, err)
	second, err := prov.CreatePayment(t.Context(), req)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.NotEmpty(t, first.ProviderPaymentID)
	assert.Equal(t, payment.ConfirmationRedirect, first.Confirmation.Type)
}

// Id события детерминированный: вебхук и сверка дедуплицируются друг с другом.
func TestMemProvider_EventIDIsDeterministic(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	id := uuid.New()

	first := prov.Event("pay-1", id, payment.EventSucceeded, 1000, "RUB")
	second := prov.Event("pay-1", id, payment.EventSucceeded, 1000, "RUB")

	assert.Equal(t, first.ProviderEventID, second.ProviderEventID)
	assert.Equal(t, "memprov:pay-1:succeeded", first.ProviderEventID)
	assert.NotEqual(t, first.ProviderEventID,
		prov.Event("pay-1", id, payment.EventCanceled, 0, "RUB").ProviderEventID)
}

func TestMemProvider_NoHolds(t *testing.T) {
	t.Parallel()

	prov := paymenttest.NewMemProvider("memprov")
	prov.NoHolds = true

	_, err := prov.Capture(t.Context(), payment.CaptureRequest{ProviderPaymentID: "pay-1"})
	require.ErrorIs(t, err, payment.ErrUnsupported)

	_, err = prov.Cancel(t.Context(), "pay-1", "k")
	require.ErrorIs(t, err, payment.ErrUnsupported)
}

// Двойники живут под -race: тесты гоняют конкурентные вебхуки, и голое поле там
// ловится гонкой, а не глазом.
func TestDoubles_AreRaceSafe(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	prov := paymenttest.NewMemProvider("memprov")
	clock := paymenttest.NewClock(now)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := intent(fmt.Sprintf("order:%d", i), fmt.Sprintf("buy-%d", i))
			_ = store.CreateIntent(context.Background(), in)
			_, _, _ = store.IntentByID(context.Background(), in.ID)
			_, _ = store.CountStuckPending(context.Background(), clock.Now())
			_, _ = prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
				IntentID: in.ID, IdempotencyKey: in.IdempotencyKey,
			})
			_ = prov.CallCount("CreatePayment")
			clock.Advance(time.Second)
			_ = store.CallCount("CreateIntent")
		}()
	}
	wg.Wait()

	assert.Equal(t, 8, store.CallCount("CreateIntent"))
	assert.Equal(t, now.Add(8*time.Second), clock.Now())
}
