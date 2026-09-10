package monolith_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestSettlerFailure_RollsBackEverything — падение хука потребителя откатывает
// ВСЁ, включая строку дедупа события.
//
// Это главный инвариант первого стыка. Останься строка дедупа после отката —
// повтор вебхука увидел бы дубль, не применил бы ничего, а провайдер получил
// бы 200 на НЕУЧТЁННУЮ оплату: деньги наши, товара нет, и следов тоже нет.
func TestSettlerFailure_RollsBackEverything(t *testing.T) {
	s := newStand(t)
	subject := registerAndConfirm(t, s)
	signIn(t, s, subject)
	intent, order := checkout(t, s)

	// Заказ исчезает — хук уронит транзакцию на первом же своём шаге.
	_, err := s.pool(t).Exec(t.Context(), "DELETE FROM shop_orders WHERE id = $1", order)
	require.NoError(t, err)

	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusServiceUnavailable, status,
		"сбой хука — 503, провайдер повторит: %s", raw(body))

	// НИ ОДНОЙ ЗАПИСИ. Ни книги, ни статуса, ни строки дедупа.
	requireCount(t, s, 0, "SELECT count(*) FROM payment_ledger")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_events")
	requireCount(t, s, 0, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 0, "SELECT count(*) FROM outbox_messages")
	requireCount(t, s, 1,
		"SELECT count(*) FROM payment_intents WHERE id = $1 AND status = 'pending'", intent)

	// ТО ЖЕ САМОЕ СОБЫТИЕ ПРИМЕНЯЕТСЯ ПОСЛЕ ПОЧИНКИ. Именно это доказывает,
	// что строка дедупа откатилась: останься она — было бы duplicate_event.
	restoreOrder(t, s, order, subject)
	status, body = s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "повтор после починки: %s", raw(body))
	require.Equal(t, "applied", body["outcome"], "то же событие применилось")

	requireCount(t, s, 1, "SELECT count(*) FROM payment_ledger WHERE intent_id = $1", intent)
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders WHERE id = $1 AND paid_at IS NOT NULL", order)
	requireCount(t, s, 2, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 1, "SELECT count(*) FROM outbox_messages WHERE kind = 'order.paid'")
}

// restoreOrder возвращает удалённый заказ на место.
func restoreOrder(t *testing.T, s *stand, order, subject uuid.UUID) {
	t.Helper()
	_, err := s.pool(t).Exec(t.Context(),
		`INSERT INTO shop_orders (id, subject_id, product_code, amount_minor, currency, created_at)
		 VALUES ($1, $2, 'course-basic', 149000, 'RUB', now())`, order, subject)
	require.NoError(t, err)
}
