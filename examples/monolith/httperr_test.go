package monolith_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// TestErrorClasses_ReachHTTP — классы ошибок доезжают до HTTP различимыми.
//
// Четвёртый стык: postgres.Sanitize → доменная ошибка → kit/errs/httperr. 503
// от недоступного хранилища и 403 от правил обязаны быть РАЗНЫМИ ответами:
// «доступа нет» при упавшей базе учит чинить права вместо базы, и инцидент
// тонет среди штатных 403.
func TestErrorClasses_ReachHTTP(t *testing.T) {
	// TTL снимка в наносекунду — это и есть режим «всегда в базу»
	// (entitlement/config.go): иначе отказ по кэшу не отличить от похода в
	// упавшее хранилище, и проверка сбоя проверяла бы кэш.
	s := newStandWith(t, map[string]string{"ENTITLEMENT_TTL": "1ns"})
	subject := registerAndConfirm(t, s)

	// 401 — сессии нет вовсе.
	status, body := s.postJSON(t, "/checkout", map[string]string{
		"product": testProduct, "idempotency_key": "k1",
	})
	require.Equal(t, http.StatusUnauthorized, status, "без сессии: %s", raw(body))

	signIn(t, s, subject)

	// 403 по ПРАВИЛУ: роль разрешает читать материал, но он не куплен.
	status, body = s.get(t, "/lesson/lesson-03")
	require.Equal(t, http.StatusForbidden, status, "материал не куплен: %s", raw(body))
	require.Equal(t, "item-not-open", body["slug"])

	providerRefusals(t, s)

	// 503 по СБОЮ: хранилище выдач пропало. Тот же маршрут, другой класс.
	_, err := s.pool(t).Exec(t.Context(), "DROP TABLE entitlement_grants")
	require.NoError(t, err)

	status, body = s.get(t, "/lesson/lesson-01")
	require.Equal(t, http.StatusServiceUnavailable, status, "хранилище недоступно: %s", raw(body))
	require.NotEqual(t, "item-not-open", body["slug"], "сбой не выдаётся за отказ")

	requireNoLeaks(t, raw(body))
}

// TestErrorBody_LeaksNothing — в теле ответа нет ни строки базы, ни логина,
// ни токена.
//
// Проверяется на ошибке, у которой всё это ЕСТЬ в причине: занятый логин
// приходит с уникального индекса, а pgconn.PgError.Detail несёт «Failing row
// contains (…)» — всю строку целиком.
func TestErrorBody_LeaksNothing(t *testing.T) {
	s := newStand(t)

	// Тело, которое не разбирается: слаг закрытого набора и request_id, и
	// больше ничего — ни текста ошибки json, ни присланных байт.
	status, body := s.postJSON(t, "/register", map[string]string{"Nope": "1"})
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "body-invalid", body["slug"])
	require.NotContains(t, raw(body), "Nope")

	// Неверный пароль: ОДИН ответ на «нет логина» и «не тот пароль».
	registerAndConfirm(t, s)
	status, body = s.postJSON(t, "/signin", map[string]string{
		"Login": testLogin, "Password": "не-тот-пароль-совсем",
	})
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, "invalid-credentials", body["slug"])
	requireNoLeaks(t, raw(body))

	status, body = s.postJSON(t, "/signin", map[string]string{
		"Login": "никого@example.test", "Password": "не-тот-пароль-совсем",
	})
	require.Equal(t, http.StatusUnauthorized, status, "несуществующий логин отвечает тем же")
	require.Equal(t, "invalid-credentials", body["slug"])
}

// requireNoLeaks — в теле нет ничего из того, что наружу не выходит.
func requireNoLeaks(t *testing.T, body string) {
	t.Helper()
	forbidden := map[string]string{
		testLogin:              "логин (персональные данные)",
		testPassword:           "пароль",
		"shop_users":           "имя таблицы",
		"entitlement_grants":   "имя таблицы",
		"auth_tokens":          "имя таблицы",
		"SQLSTATE":             "код Postgres",
		"Failing row contains": "содержимое строки",
		"pgx":                  "имя драйвера",
		"relation":             "текст Postgres",
	}
	for needle, why := range forbidden {
		require.NotContains(t, body, needle,
			"в теле ответа не должно быть: %s; тело: %s", why, body)
	}
}

// providerRefusals — отказ провайдера доезжает до HTTP СВОИМ классом по
// обоим путям, «не умеет» — своим, а временный сбой остаётся 503.
//
// До пересадки этого теста не было, и маскировку никто не заметил: отказ,
// пришедший ОШИБКОЙ адаптера, заворачивался в ErrUnavailable, а таблица
// ответов проверяет недоступность первой — правило provider-rejected на этом
// пути было мёртвым, и 503 учил клиента повторять окончательное «нет».
func providerRefusals(t *testing.T, s *stand) {
	t.Helper()
	p := s.app.Provider()

	// Путь 1: провайдер ответил статусом failed. Ссылку заказа тест не знает —
	// она рождается в ручке, — поэтому RejectNext, а не RejectReference.
	p.RejectNext()
	requireRefusal(t, s, "reject-status", http.StatusConflict, "provider-rejected")

	// Путь 2: адаптер вернул отказ ОШИБКОЙ — тот самый, что был замаскирован.
	p.SetCreateErr(payment.ErrProviderRejected)
	requireRefusal(t, s, "reject-error", http.StatusConflict, "provider-rejected")

	// «Не умеет» — 501: без своего правила он упал бы в 500.
	p.SetCreateErr(payment.ErrUnsupported)
	requireRefusal(t, s, "unsupported", http.StatusNotImplemented, "payment-unsupported")

	// Временный сбой ОСТАЁТСЯ 503: снятие обёртки не должно было задеть
	// «ответа нет» — там повтор законен.
	p.SetCreateErr(paymenttest.ErrProviderDown)
	requireRefusal(t, s, "transient", http.StatusServiceUnavailable, "payment-unavailable")

	// createErr проверяется раньше повтора по ключу: не сбросить — и
	// следующий checkout в этом тесте упал бы по чужой причине.
	p.SetCreateErr(nil)
}

// requireRefusal — checkout под новым ключом и его ответ.
func requireRefusal(t *testing.T, s *stand, key string, wantStatus int, wantSlug string) {
	t.Helper()
	status, body := s.postJSON(t, "/checkout", map[string]string{
		"product": testProduct, "idempotency_key": "refusal-" + key,
	})
	require.Equal(t, wantStatus, status, "%s: %s", key, raw(body))
	require.Equal(t, wantSlug, body["slug"], key)
	requireNoLeaks(t, raw(body))
}
