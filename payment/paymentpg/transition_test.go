package paymentpg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// Оплатить мимо книги нельзя, и держит это база: у succeeded обязан быть момент
// зачисления, а назначает его только ApplyEvent. Иначе смена статуса — операция,
// которая деньгами не является, — могла бы их двигать.
func TestStore_Transition_CannotSettleWithoutLedger(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	in := mustCreate(t, store, intent())
	toPending(t, store, in)

	_, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:   in.ID,
		ExpectFrom: expectFrom(payment.StatusSucceeded),
		To:         payment.StatusSucceeded,
		Now:        testNow(),
	})
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Contains(t, err.Error(), "payment_intents_settled_chk")

	after, _, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Equal(t, payment.StatusPending, after.Status)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_ledger`))
}

// Пустое подтверждение не затирает уже выданное: ссылка на оплату у плательщика
// на руках, и переход created → pending её не отменяет.
func TestStore_Transition_KeepsIssuedConfirmation(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t, paymentpg.Options{})
	in := mustCreate(t, store, intent())
	toPending(t, store, in)

	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:   in.ID,
		ExpectFrom: expectFrom(payment.StatusCanceled),
		To:         payment.StatusCanceled,
		Now:        testNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Equal(t, payment.ConfirmationRedirect, res.Intent.Confirmation.Type)
	assert.Equal(t, "pay_"+in.ID.String(), res.Intent.ProviderPaymentID)
	assert.NotEmpty(t, res.Intent.Items, "состав едет вместе с намерением")
}
