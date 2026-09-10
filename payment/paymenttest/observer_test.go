package paymenttest_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Двойник отдаёт копию: правка полученного среза его состояние не меняет.
func TestObserver_OutcomesIsACopy(t *testing.T) {
	t.Parallel()

	obs := paymenttest.NewObserver()
	obs.Outcome(t.Context(), payment.OpStart, payment.ReasonCreated)

	got := obs.Outcomes()
	got[0].Reason = payment.ReasonStoreError

	assert.Equal(t, []paymenttest.Observed{{Op: payment.OpStart, Reason: payment.ReasonCreated}}, obs.Outcomes())
}

// Сервис зовёт наблюдателя из конкурентных вебхуков.
func TestObserver_IsRaceSafe(t *testing.T) {
	t.Parallel()

	obs := paymenttest.NewObserver()
	const workers, each = 8, 50

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				obs.Outcome(context.Background(), payment.OpWebhook, payment.ReasonSettled)
				_ = obs.Outcomes()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, obs.Outcomes(), workers*each)
}
