package monolith_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	testLogin    = "buyer@example.test"
	testPassword = "q7-Lagoon-Quartz-Marmot-91"
	testProduct  = "course-basic"
)

// TestMonolith_EndToEnd — сквозной путь потребителя на всех модулях сразу.
//
// ЭТО ГЕЙТ ПЕРЕД ТЕГАМИ, А НЕ ВИТРИНА. Каждый модуль протестирован сам по
// себе; здесь проверяется то, чего не проверял никто: сходятся ли их порты у
// живого потребителя. Один тест и один сценарий — потому что интересны именно
// переходы между модулями, а не отдельные шаги.
func TestMonolith_EndToEnd(t *testing.T) {
	s := newStand(t)

	subject := registerAndConfirm(t, s)
	signIn(t, s, subject)
	intent, order := checkout(t, s)
	settle(t, s, intent, order)
	deliverPaidLetter(t, s, order)
	openLesson(t, s)
	uploadFile(t, s)
	runEveryJob(t, s)
	checkMetrics(t, s)
}

// openLesson — то, ради чего платили: купленный материал открывается, а
// некупленный нет.
//
// Обе оси сразу: роль даёт разрешение «читать материал», хук authz
// спрашивает у entitlement, куплен ли ИМЕННО ЭТОТ (authz/ports.go, Policy).
func openLesson(t *testing.T, s *stand) {
	t.Helper()

	status, body := s.get(t, "/lesson/lesson-01")
	require.Equal(t, http.StatusOK, status, "купленный материал открыт: %s", raw(body))
	require.Equal(t, "lesson-01", body["lesson"])

	status, body = s.get(t, "/lesson/lesson-03")
	require.Equal(t, http.StatusForbidden, status, "чужой материал закрыт: %s", raw(body))
	require.Equal(t, "item-not-open", body["slug"])
}

// runEveryJob — все шесть фоновых задач отрабатывают на живой базе.
//
// Не «покрытие»: payments_reconcile и objectstore_collect иначе не звались бы
// в тесте ни разу, и их проводка (курсор сверки, порт Owned уборщика)
// проверялась бы только компилятором.
func runEveryJob(t *testing.T, s *stand) {
	t.Helper()
	for _, name := range []string{
		"mail_deliver", "outbox_drain", "payments_reconcile",
		"auth_sweep", "objectstore_collect", "gauges_snapshot",
	} {
		_, err := s.app.Jobs().RunNow(t.Context(), name)
		require.NoError(t, err, "задача %s", name)
	}
}

// registerAndConfirm — шаги 1 и 2: регистрация, письмо со ссылкой, переход по
// ней, повтор той же ссылки.
func registerAndConfirm(t *testing.T, s *stand) uuid.UUID {
	t.Helper()

	status, body := s.postJSON(t, "/register", map[string]string{
		"Login": testLogin, "Password": testPassword,
	})
	require.Equal(t, http.StatusAccepted, status, "регистрация отвечает «принято»: %s", raw(body))

	// СТРОКА ТОКЕНА И СТРОКА ПИСЬМА ЛЕЖАТ ВМЕСТЕ. Это стык session.Tokens:
	// письмо без токена — ссылка в никуда, токен без письма — тишина после
	// «мы отправили вам ссылку».
	requireCount(t, s, 1, "SELECT count(*) FROM auth_tokens WHERE purpose = 'verify'")
	requireCount(t, s, 1, "SELECT count(*) FROM email_outbox WHERE kind = 'verify'")

	s.runJob(t, "mail_deliver")
	letter := box.waitFor(t, "Подтвердите адрес")
	require.Equal(t, testLogin, letter.To[0].Address)
	rawToken := tokenFromLetter(t, letter.Text)

	// В БАЗЕ ЛЕЖИТ HMAC, А НЕ ТОКЕН. Дамп базы не даёт живой ссылки.
	requireCount(t, s, 1,
		"SELECT count(*) FROM auth_tokens WHERE token_hash = $1", hashOf(t, rawToken))
	requireCount(t, s, 0,
		"SELECT count(*) FROM auth_tokens WHERE token_hash = $1", rawToken)

	status, body = s.get(t, "/confirm?token="+rawToken)
	require.Equal(t, http.StatusOK, status, "переход по ссылке: %s", raw(body))
	subject := uuid.MustParse(str(t, body, "subject_id"))

	requireCount(t, s, 1, "SELECT count(*) FROM shop_users WHERE id = $1 AND verified", subject)

	// ПОВТОР ТОЙ ЖЕ ССЫЛКИ НЕ ПРОХОДИТ: одноразовость решает база одним
	// UPDATE с предикатом, а не парой «прочитал, потом записал».
	status, body = s.get(t, "/confirm?token="+rawToken)
	require.Equal(t, http.StatusBadRequest, status, "повтор ссылки: %s", raw(body))
	return subject
}

// signIn — шаг 3: вход, сессионная кука и отдельная кука CSRF.
func signIn(t *testing.T, s *stand, subject uuid.UUID) {
	t.Helper()
	s.grantRole(t, subject)

	status, body := s.postJSON(t, "/signin", map[string]string{
		"Login": testLogin, "Password": testPassword,
	})
	require.Equal(t, http.StatusOK, status, "вход: %s", raw(body))
	require.Equal(t, subject.String(), body["subject_id"])

	// СЫРОЙ ТОКЕН УЕЗЖАЕТ ТОЛЬКО В КУКУ. В теле ответа его нет.
	require.NotContains(t, raw(body), "token")

	session, csrf := s.cookies(t)
	require.True(t, session.HttpOnly, "сессионная кука недоступна JS")
	require.False(t, csrf.HttpOnly, "кука CSRF ДОСТУПНА JS: иначе сравнивать нечего")
	require.NotEqual(t, session.Value, csrf.Value, "куки различны")
	requireCount(t, s, 1, "SELECT count(*) FROM auth_sessions WHERE subject_id = $1", subject)

	// ЖУРНАЛ БЕЗОПАСНОСТИ ВЕДЁТСЯ. Ни пароля, ни логина в записи нет: логин
	// это персональные данные, а журнал живёт дольше всего.
	requireCount(t, s, 1,
		"SELECT count(*) FROM audit_events WHERE action = 'auth.signed_in' AND outcome = 'success'")
	requireCount(t, s, 0,
		"SELECT count(*) FROM audit_events WHERE audit_events::text LIKE '%' || $1 || '%'", testLogin)
}

// checkout — шаг 4: намерение и его идемпотентность.
func checkout(t *testing.T, s *stand) (intent, order uuid.UUID) {
	t.Helper()
	key := "buy-" + uuid.NewString()

	status, first := s.postJSON(t, "/checkout", map[string]string{
		"product": testProduct, "idempotency_key": key,
	})
	require.Equal(t, http.StatusOK, status, "checkout: %s", raw(first))
	require.True(t, boolOf(t, first, "created"), "первый вызов создаёт намерение")

	// ТОТ ЖЕ КЛЮЧ — ТО ЖЕ НАМЕРЕНИЕ. Второй заказ при повторе не заводится:
	// иначе ретрай из другой вкладки означал бы второе списание.
	status, second := s.postJSON(t, "/checkout", map[string]string{
		"product": testProduct, "idempotency_key": key,
	})
	require.Equal(t, http.StatusOK, status, "повтор checkout: %s", raw(second))
	require.Equal(t, first["intent_id"], second["intent_id"], "то же намерение")
	require.Equal(t, first["order_id"], second["order_id"], "тот же заказ")
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders")
	requireCount(t, s, 1, "SELECT count(*) FROM payment_intents")

	return uuid.MustParse(str(t, first, "intent_id")), uuid.MustParse(str(t, first, "order_id"))
}

// settle — шаг 5: вебхук, зачисление и ВСЕ эффекты потребителя одной
// транзакцией.
func settle(t *testing.T, s *stand, intent, order uuid.UUID) {
	t.Helper()

	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "вебхук: %s", raw(body))
	require.Equal(t, "applied", body["outcome"])

	// ЧЕТЫРЕ ЗАПИСИ ОДНИМ КОММИТОМ: книга платежей (тулкит), заказ, право и
	// событие outbox (потребитель). Это первый стык примера.
	requireCount(t, s, 1,
		"SELECT count(*) FROM payment_ledger WHERE intent_id = $1 AND kind = 'capture'", intent)
	requireCount(t, s, 1, "SELECT count(*) FROM shop_orders WHERE id = $1 AND paid_at IS NOT NULL", order)
	requireCount(t, s, 2, "SELECT count(*) FROM entitlement_grants")
	requireCount(t, s, 1, "SELECT count(*) FROM outbox_messages WHERE kind = 'order.paid'")

	// МОМЕНТ ВЫДАЧИ — ТОТ, ЧТО ПРИШЁЛ ПАРАМЕТРОМ, а не now() адаптера: у
	// каждой выдачи granted_at равен моменту записи книги. Без этого
	// утверждения адаптер с DEFAULT now() прошёл бы весь сценарий, и
	// требование эталонной схемы не сторожилось бы ничем.
	requireCount(t, s, 2, `SELECT count(*) FROM entitlement_grants g
		 JOIN payment_ledger l ON l.intent_id = $1 AND l.kind = 'capture'
		 WHERE g.granted_at = l.created_at`, intent)

	// ПОВТОРНАЯ ДОСТАВКА — ЭТО НОРМА at-least-once, а не вторая оплата.
	status, body = s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "повтор вебхука: %s", raw(body))
	require.Equal(t, "duplicate_event", body["outcome"])
	requireCount(t, s, 1, "SELECT count(*) FROM payment_ledger WHERE intent_id = $1", intent)
	requireCount(t, s, 1, "SELECT count(*) FROM outbox_messages WHERE kind = 'order.paid'")
}

// deliverPaidLetter — шаг 6: outbox разбирает событие, почта отправляет письмо.
func deliverPaidLetter(t *testing.T, s *stand, order uuid.UUID) {
	t.Helper()

	processed, err := s.app.Jobs().RunNow(t.Context(), "outbox_drain")
	require.NoError(t, err, "разбор outbox")
	require.Equal(t, 1, processed, "разобрано одно событие")
	requireCount(t, s, 1,
		"SELECT count(*) FROM outbox_messages WHERE kind = 'order.paid' AND status = 'done'")
	requireCount(t, s, 1, "SELECT count(*) FROM email_outbox WHERE kind = 'payment_receipt'")

	s.runJob(t, "mail_deliver")
	letter := box.waitFor(t, "Заказ оплачен")
	require.Contains(t, letter.Text, order.String())

	// ХЕНДЛЕР ИДЕМПОТЕНТЕН: повторный прогон не удвоит письмо. Доставка
	// at-least-once, и прошлая попытка могла оставить эффект.
	require.NoError(t, s.replayOutbox(t))
	s.runJob(t, "mail_deliver")
	require.Equal(t, 1, box.count(t, "Заказ оплачен"), "письмо об оплате одно")
}

// uploadFile — шаг 7: объект в fs, ключ нашей постройки.
func uploadFile(t *testing.T, s *stand) {
	t.Helper()
	const userName = "секретное-имя-пользователя.png"

	status, body := s.upload(t, userName, pngBody())
	require.Equal(t, http.StatusCreated, status, "загрузка: %s", raw(body))

	key := str(t, body, "key")
	require.True(t, strings.HasPrefix(key, "uploads/"), "ключ строит хранилище: %q", key)
	require.True(t, strings.HasSuffix(key, ".png"), "расширение по СОДЕРЖИМОМУ: %q", key)

	// ИМЯ ФАЙЛА ПОЛЬЗОВАТЕЛЯ В КЛЮЧ НЕ ПОПАДАЕТ. Оно данные, а не путь.
	require.NotContains(t, key, "секретное")
	require.NotContains(t, key, userName)
	requireCount(t, s, 1,
		"SELECT count(*) FROM shop_uploads WHERE object_key = $1 AND original_name = $2", key, userName)
}

// checkMetrics — шаг 8: /metrics отдаёт то, на что вешают алерты.
func checkMetrics(t *testing.T, s *stand) {
	t.Helper()
	body := s.scrape(t)
	// payments_total — наблюдатель проведён: все пары op × reason
	// рождаются нулём при сборке, поэтому ряд есть и до первой оплаты.
	for _, want := range []string{
		"build_info", "cron_runs", "outbox_pending", "emails_sent", "payments_total",
		"payment_provider_calls_total", "payment_intents_stuck", "payment_drift",
	} {
		require.Contains(t, body, want, "в /metrics нет %s", want)
	}

	// Одна оплата — один вызов создания платежа, и он успешный: повтор
	// checkout с тем же ключом до провайдера не доходит.
	requireMetric(t, body, "payment_provider_calls_total",
		map[string]string{"type": "create_payment", "result": "ok"}, 1)
}

// providerBody — успешная оплата в той форме, в какой её пришлёт провайдер.
//
// ProviderEventID детерминированный: вебхук и сверка обязаны дедуплицироваться
// друг с другом, а не применять одно событие дважды (payment/intent.go).
func providerBody(intent uuid.UUID) map[string]any {
	const kind = "succeeded"
	return map[string]any{
		"event_id":     "ev-" + intent.String() + "-" + kind,
		"payment_id":   "pay-" + intent.String(),
		"intent_id":    intent.String(),
		"type":         kind,
		"amount_minor": 149000,
		"currency":     "RUB",
	}
}
