package monolith_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/payment/paymenttest"

	"github.com/nrect/rebar/examples/monolith"
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

// commitRefusal — текст отказа отложенного триггера refuseCommit.
const commitRefusal = "фиксация отбита тестом"

// TestSettlerCommitFailure_RollsBackGrants — зачисление падает на COMMIT, когда
// хук уже пометил заказ, выдал права и положил событие: не остаётся ничего, а
// повтор вебхука после починки применяет оплату.
//
// TestSettlerFailure_RollsBackEverything роняет хук на первом шаге, до выдачи,
// и права, выданные мимо транзакции зачисления (пул вместо WithTx), не видит.
func TestSettlerCommitFailure_RollsBackGrants(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))
	intent, order := checkout(t, s)
	refuseCommit(t, s)

	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusServiceUnavailable, status, "сбой фиксации — 503, провайдер повторит: %s", raw(body))
	requireRefusalLogged(t, s)

	requireCount(t, s, 0, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 0, "SELECT count(*) FROM outbox_messages")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_ledger")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_events")
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders WHERE id = $1 AND paid_at IS NULL", order)

	allowCommit(t, s)
	status, body = s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "повтор после снятия триггера: %s", raw(body))
	require.Equal(t, "applied", body["outcome"])
	requireCount(t, s, 2, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders WHERE id = $1 AND paid_at IS NOT NULL", order)
}

// TestRefundCommitFailure_KeepsGrants — возврат падает на COMMIT, когда хук уже
// отозвал права и положил событие: права на месте, записи возврата нет; повтор
// с тем же ключом после починки права отзывает.
//
// Отзыв мимо транзакции (пул вместо WithTx) пережил бы откат: доступ снят, а
// возврата в книге нет.
func TestRefundCommitFailure_KeepsGrants(t *testing.T) {
	s := newStand(t)
	signIn(t, s, registerAndConfirm(t, s))
	intent, _ := checkout(t, s)
	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "оплата до возврата: %s", raw(body))
	requireCount(t, s, 2, "SELECT count(*) FROM entitlement_grants")

	refund := payment.RefundRequest{
		IntentID: intent, AmountMinor: 149000, ActorID: uuid.New(),
		IdempotencyKey: "refund-" + intent.String(),
	}
	refuseCommit(t, s)
	_, _, err := s.app.Refund(t.Context(), refund)
	require.ErrorIs(t, err, payment.ErrUnavailable, "сбой фиксации возврата — недоступность")
	require.ErrorContains(t, err, commitRefusal, "отбита фиксация, а не шаг до хука")
	requireCount(t, s, 2, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_ledger WHERE kind = 'refund'")
	requireCount(t, s, 0, "SELECT count(*) FROM outbox_messages WHERE kind = 'order.refunded'")

	allowCommit(t, s)
	_, reason, err := s.app.Refund(t.Context(), refund)
	require.NoError(t, err, "повтор возврата с тем же ключом")
	require.Equal(t, payment.ReasonRefunded, reason)
	requireCount(t, s, 0, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 1, "SELECT count(*) FROM payment_ledger WHERE kind = 'refund'")
	requireCount(t, s, 1, "SELECT count(*) FROM outbox_messages WHERE kind = 'order.refunded'")
}

// refuseCommit ставит на outbox_messages отложенный триггер: вставка проходит, а
// транзакция падает на COMMIT — после ВСЕХ шагов хука, в каком бы порядке они ни
// шли. Эффект хука, записанный мимо транзакции, переживает откат.
func refuseCommit(t *testing.T, s *stand) {
	t.Helper()
	_, err := s.pool(t).Exec(t.Context(), `
		CREATE OR REPLACE FUNCTION refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION '`+commitRefusal+`'; END $$;
		CREATE CONSTRAINT TRIGGER refuse_commit_trg AFTER INSERT ON outbox_messages
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_commit()`)
	require.NoError(t, err, "отложенный триггер")
}

// allowCommit снимает триггер refuseCommit.
func allowCommit(t *testing.T, s *stand) {
	t.Helper()
	_, err := s.pool(t).Exec(t.Context(), `DROP TRIGGER refuse_commit_trg ON outbox_messages`)
	require.NoError(t, err, "снятие триггера")
}

// requireRefusalLogged — 503 дала фиксация, а не шаг до хука: хук отработал
// целиком, и откатывать было что.
func requireRefusalLogged(t *testing.T, s *stand) {
	t.Helper()
	for _, rec := range s.logs.records(t, "http error") {
		if text, ok := rec["error"].(string); ok && strings.Contains(text, commitRefusal) {
			return
		}
	}
	t.Fatalf("в логе нет 5xx с отказом фиксации %q", commitRefusal)
}

// TestSettlerSlugError_KeepsWebhookRetryable — SlugError хука зачисления не
// перебивает недоступность ядра: вебхук отвечает 503, и повтор провайдера
// применяет ту же оплату.
//
// Ядро заворачивает ошибку хука в payment.ErrUnavailable. До kit v0.3.0
// errs.KindOf искал SlugError по всей цепочке раньше KindError, и errs.Conflict
// хука давала 409: провайдер переставал повторять, оплата терялась навсегда.
// Хук здесь свой: shoppg.Settler SlugError не возвращает, и на нём дефект не
// проявился бы.
func TestSettlerSlugError_KeepsWebhookRetryable(t *testing.T) {
	s := newStand(t)
	hook := &refusingSettler{err: errs.Conflict("seat-taken")}
	provider := paymenttest.NewMemProvider(monolith.ProviderName)
	pay := payment.NewService(paymentpg.New(s.pool(t), paymentpg.Options{Settler: hook}),
		provider, paymenttest.NewObserver(), monolith.PaymentConfig())
	webhook := monolith.WebhookHandler(pay, provider, s.log)
	intent := pendingIntent(t, pay)

	status, body := serveJSON(t, webhook, providerBody(intent))
	require.Equal(t, http.StatusServiceUnavailable, status,
		"отказ хука — 503, провайдер повторит: %s", raw(body))
	require.Equal(t, "unavailable", body["slug"], "слаг хука наружу не выходит")
	require.Equal(t, 1, hook.calls, "ответ дал хук, а не шаг до него")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_events")
	requireCount(t, s, 0, "SELECT count(*) FROM payment_ledger")

	// 503 ОСТАВИЛ ОПЛАТУ НЕУЧТЁННОЙ, А НЕ ПОТЕРЯННОЙ: повтор применяется.
	hook.err = nil
	status, body = serveJSON(t, webhook, providerBody(intent))
	require.Equal(t, http.StatusOK, status, "повтор провайдера: %s", raw(body))
	require.Equal(t, "applied", body["outcome"])
	requireCount(t, s, 1, "SELECT count(*) FROM payment_ledger WHERE intent_id = $1", intent)
}

// TestWebhook_AnswersClassWithoutTranslate — ручка вебхука отвечает классом,
// мимо словаря продукта: правило item-not-open совпадает с ошибкой хука глубоко
// под payment.ErrUnavailable, и продуктовый ответчик отдал бы провайдеру 403
// вместо 503 (условие держит TestResponders_WebhookSkipsProductRules).
func TestWebhook_AnswersClassWithoutTranslate(t *testing.T) {
	s := newStand(t)
	hook := &refusingSettler{err: fmt.Errorf("%w: предмет снят с продажи", monolith.ErrItemNotOpen)}
	provider := paymenttest.NewMemProvider(monolith.ProviderName)
	pay := payment.NewService(paymentpg.New(s.pool(t), paymentpg.Options{Settler: hook}),
		provider, paymenttest.NewObserver(), monolith.PaymentConfig())
	intent := pendingIntent(t, pay)

	status, body := serveJSON(t, monolith.WebhookHandler(pay, provider, s.log), providerBody(intent))
	require.Equal(t, http.StatusServiceUnavailable, status,
		"провайдеру — класс, а не слаг продукта: %s", raw(body))
	require.Equal(t, "unavailable", body["slug"])
	require.Equal(t, 1, hook.calls, "ответ дал хук, а не шаг до него")
}

// restoreOrder возвращает удалённый заказ на место.
func restoreOrder(t *testing.T, s *stand, order, subject uuid.UUID) {
	t.Helper()
	_, err := s.pool(t).Exec(t.Context(),
		`INSERT INTO shop_orders (id, subject_id, product_code, amount_minor, currency, created_at)
		 VALUES ($1, $2, 'course-basic', 149000, 'RUB', now())`, order, subject)
	require.NoError(t, err)
}

// refusingSettler — хук зачисления, отвечающий err; nil пропускает зачисление.
type refusingSettler struct {
	err   error
	calls int
}

func (h *refusingSettler) OnSettled(context.Context, pgx.Tx, payment.Intent, payment.LedgerEntry) error {
	h.calls++
	return h.err
}

func (*refusingSettler) OnRefunded(context.Context, pgx.Tx, payment.Intent, payment.LedgerEntry) error {
	return nil
}

// pendingIntent — намерение на товар каталога, у провайдера уже есть платёж.
func pendingIntent(t *testing.T, pay *payment.Service) uuid.UUID {
	t.Helper()
	const amount = 149000
	res, _, err := pay.Start(t.Context(), payment.StartRequest{
		PayerID:   uuid.New(),
		Reference: uuid.NewString(),
		Items: []payment.OrderItem{{
			ProductID: testProduct, Title: "Базовый курс", AmountMinor: amount, Quantity: 1,
		}},
		AmountMinor:    amount,
		Currency:       "RUB",
		AutoCapture:    true,
		IdempotencyKey: "slug-hook-" + uuid.NewString(),
	})
	require.NoError(t, err)
	return res.Intent.ID
}

// serveJSON — POST тела прямо в обработчик и ответ с разобранным телом.
func serveJSON(t *testing.T, h http.Handler, body any) (status int, out map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook",
		bytes.NewReader(payload)))
	out = map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "тело: %s", rec.Body.String())
	out["__raw"] = rec.Body.String()
	return rec.Code, out
}
