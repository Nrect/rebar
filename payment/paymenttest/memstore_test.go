package paymenttest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Ручки хранилища правятся на ходу: тест потребителя меняет их, пока ручка его
// HTTP-сервера в другой горутине зовёт стор. Под -race это обязано быть чисто —
// иначе краснело бы у потребителя, а не у нас.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	ctx := context.Background()
	stop := make(chan struct{})
	served := make(chan struct{})

	go func() { // ручка сервера
		defer close(served)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			in := intent(fmt.Sprintf("order:%d", i), fmt.Sprintf("buy-%d", i))
			_ = store.CreateIntent(ctx, in)
			_, _, _ = store.IntentByKey(ctx, in.PayerID, in.IdempotencyKey)
			_, _, _ = store.IntentByID(ctx, in.ID)
			_, _ = store.Transition(ctx, payment.TransitionRequest{
				IntentID: in.ID, ExpectFrom: []payment.Status{payment.StatusCreated},
				To: payment.StatusPending, Now: now,
			})
			_, _ = store.ApplyEvent(ctx, payment.ApplyEventRequest{
				IntentID: in.ID, Event: payment.Event{Provider: "memprov", ProviderEventID: fmt.Sprintf("ev-%d", i)},
				Now: now,
			})
			_, _ = store.Ledger(ctx, in.ID)
			_, _ = store.StalePending(ctx, now.Add(time.Hour), payment.IntentCursor{}, 10)
			_, _ = store.CountStuckPending(ctx, now.Add(time.Hour))
			_, _ = store.Drift(ctx, now, 10)
		}
	}()

	hook := func(payment.Intent, payment.LedgerEntry) error { return nil }
	for i := range 200 { // горутина теста
		var fail error
		if i%2 == 0 {
			fail = paymenttest.ErrStore
		}
		store.SetErr(fail)
		store.SetRaceOnce(i%3 == 0)
		store.SetRefundTooLargeOnce(i%5 == 0)
		store.SetDriftRecords([]payment.DriftRecord{{IntentID: uuid.New(), Kind: payment.DriftRefundOverCapture}})
		store.SetOnSettled(hook)
		store.SetOnRefunded(hook)
		store.SeedKey(uuid.New(), "dangling", uuid.New())
		store.Seed(intent(fmt.Sprintf("seed:%d", i), fmt.Sprintf("seed-%d", i)))
		store.SeedEntry(payment.LedgerEntry{ID: uuid.New(), Kind: payment.LedgerCapture})
		if i%50 == 0 {
			store.ClearEntries()
		}
		_, _ = store.Entries(), store.LastApplySeq()
		_ = store.CallCount("CreateIntent")
		_ = store.Deliveries(payment.Event{Provider: "memprov", ProviderEventID: "ev-1"})
		_ = store.EntriesOf(uuid.New(), payment.LedgerCapture)
	}
	close(stop)
	<-served
}

// Двойник держит свою копию: правка среза после SetDriftRecords и правка
// полученной книги его состояние не меняют.
func TestMemStore_KnobsAndReadsAreCopies(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	records := []payment.DriftRecord{{Reference: "order:1", Kind: payment.DriftSucceededNoCapture}}
	store.SetDriftRecords(records)
	records[0].Reference = "order:tampered"

	got, err := store.Drift(t.Context(), now, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "order:1", got[0].Reference)

	store.SeedEntry(payment.LedgerEntry{ID: uuid.New(), Kind: payment.LedgerCapture, IdempotencyKey: "k-1"})
	entries := store.Entries()
	entries[0].IdempotencyKey = "tampered"
	assert.Equal(t, "k-1", store.Entries()[0].IdempotencyKey)
}
