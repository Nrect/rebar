package monolith_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment/paymenttest"
)

// TestCheckout_KeyReuseIsConflict — повтор ключа разбирает payment.Start: другой
// товар под тем же ключом — 409, а не прежнее намерение с чужой ценой и ссылкой.
func TestCheckout_KeyReuseIsConflict(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))
	key := "reuse-" + uuid.NewString()

	status, body := s.postJSON(t, "/checkout", checkoutBody(testProduct, key))
	require.Equal(t, http.StatusOK, status, "первая покупка: %s", raw(body))

	status, body = s.postJSON(t, "/checkout", checkoutBody("course-pro", key))
	require.Equal(t, http.StatusConflict, status, "другой товар под тем же ключом: %s", raw(body))
	require.Equal(t, "conflict", body["slug"])
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders")
	requireCount(t, s, 1, "SELECT count(*) FROM payment_intents")
}

// TestCheckout_RetryAfterRejectionStaysRejected — повтор ключа после отказа
// провайдера отвечает тем же отказом, а не 200 без ссылки на оплату.
func TestCheckout_RetryAfterRejectionStaysRejected(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))
	s.app.Provider().RejectNext()
	key := "rejected-" + uuid.NewString()

	for attempt := range 2 {
		status, body := s.postJSON(t, "/checkout", checkoutBody(testProduct, key))
		require.Equal(t, http.StatusConflict, status, "попытка %d: %s", attempt, raw(body))
		require.Equal(t, "provider-rejected", body["slug"], "попытка %d", attempt)
	}
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders")
}

// TestCheckout_ExpiredAttemptAsksNewKey — повтор ключа протухшей попытки — 409
// payment-closed: клиенту нужен новый ключ, а не оплата по вчерашней цене.
func TestCheckout_ExpiredAttemptAsksNewKey(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))
	key := "expired-" + uuid.NewString()

	// Провайдер не ответил: намерение осталось created, и его состарили за TTL.
	s.app.Provider().SetCreateErr(paymenttest.ErrProviderDown)
	status, body := s.postJSON(t, "/checkout", checkoutBody(testProduct, key))
	require.Equal(t, http.StatusServiceUnavailable, status, "провайдер недоступен: %s", raw(body))
	s.app.Provider().SetCreateErr(nil)
	_, err := s.pool(t).Exec(t.Context(),
		`UPDATE payment_intents SET created_at = created_at - interval '1 hour',
		 updated_at = updated_at - interval '1 hour', expires_at = expires_at - interval '1 hour'
		 WHERE status = 'created'`)
	require.NoError(t, err)

	status, body = s.postJSON(t, "/checkout", checkoutBody(testProduct, key))
	require.Equal(t, http.StatusConflict, status, "протухшая попытка: %s", raw(body))
	require.Equal(t, "payment-closed", body["slug"])
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders")
}

// TestCheckout_BadKeyLeavesNoOrder — негодный ключ отвергается до заказа: сироты
// в shop_orders нет.
func TestCheckout_BadKeyLeavesNoOrder(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))

	status, body := s.postJSON(t, "/checkout", checkoutBody(testProduct, "   "))
	require.Equal(t, http.StatusBadRequest, status, "пустой ключ: %s", raw(body))
	require.Equal(t, "incorrect-input", body["slug"])
	requireCount(t, s, 0, "SELECT count(*) FROM shop_orders")
}

// checkoutBody — тело checkout.
func checkoutBody(product, key string) map[string]string {
	return map[string]string{"product": product, "idempotency_key": key}
}
