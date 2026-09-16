package monolith

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/payment"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// quiet — логгер ответчиков в табличных тестах: записи здесь не проверяются.
var quiet = slog.New(slog.DiscardHandler)

// TestTranslate_SentinelsReachHTTP — итоговые статус и слаг каждой sentinel,
// которую монолит может отдать наружу: гейт «порты сходятся у потребителя».
// Ручкам людей отвечает продуктовый ответчик, вебхуку — ответчик классом.
func TestTranslate_SentinelsReachHTTP(t *testing.T) {
	check := func(respond *httperr.Responder, all []outcome) {
		for _, o := range all {
			status, slug := answerOf(t, respond, o.err)
			assert.Equal(t, o.status, status, "%s: %v", o.route, o.err)
			assert.Equal(t, o.slug, slug, "%s: %v", o.route, o.err)
		}
	}
	check(newResponder(quiet), outcomes())
	check(newClassResponder(quiet), machineOutcomes())
}

// TestTranslate_NoRedundantRule — правило, без которого ответ не хуже, лишнее:
// таблица не разрастается обратно.
//
// Без правила httperr отвечает статусом класса и слагом — именем класса. Тот же
// ответ — находка всегда; тот же статус оправдан только строкой clientActs, а
// понижение класса до 500 — только строкой downgrades.
func TestTranslate_NoRedundantRule(t *testing.T) {
	respond, acts, down := newResponder(quiet), clientActs(), downgrades()
	answered := make(map[string]bool, len(acts))
	lowered := make(map[error]bool, len(down))
	for _, r := range rules() {
		status, slug := answerOf(t, respond, r.sentinel)
		answered[slug] = true
		kind := errs.KindOf(r.sentinel)
		classStatus, classSlug := httperr.StatusOf(kind), string(kind)
		if kind == errs.KindUnknown {
			classSlug = httperr.DefaultInternalSlug
		}
		switch {
		case status == classStatus && slug == classSlug:
			t.Errorf("правило лишнее: %q отвечает тем же, что без него (%d %s)", r.sentinel, status, slug)
		case status == classStatus && acts[slug] == "":
			t.Errorf("правило лишнее: %q отвечает статусом класса %d, а чем слаг %q полезнее %q, в clientActs не сказано",
				r.sentinel, status, slug, classSlug)
		case status == http.StatusInternalServerError:
			lowered[r.sentinel] = true
			if down[r.sentinel] == "" {
				t.Errorf("правило понижает %q с %d %s до 500, а почему класс модуля здесь соврал бы, в downgrades не сказано",
					r.sentinel, classStatus, classSlug)
			}
		}
	}
	for slug := range acts {
		if !answered[slug] {
			t.Errorf("clientActs объясняет слаг %q, которого не отдаёт ни одно правило", slug)
		}
	}
	for sentinel := range down {
		if !lowered[sentinel] {
			t.Errorf("downgrades объясняет понижение %q, которого нет в таблице", sentinel)
		}
	}
}

// clientActs — что клиент делает по слагу продукта иначе, чем по имени класса.
// Правило со статусом класса без строки здесь лишнее.
func clientActs() map[string]string {
	return map[string]string{
		"invalid-credentials":   "остаётся на форме входа и просит пароль заново, а по unauthenticated уводит на вход",
		"email-not-verified":    "зовёт подтвердить адрес по письму вместо «нет доступа»",
		"token-invalid":         "говорит, что ссылка из письма мертва: исправлять во вводе нечего",
		"login-invalid":         "подсвечивает поле логина, а не форму целиком",
		"password-too-short":    "просит пароль длиннее",
		"password-too-long":     "просит пароль короче",
		"password-too-weak":     "просит пароль, который труднее угадать",
		"item-not-open":         "предлагает купить материал вместо «нет доступа»",
		"payment-closed":        "начинает новую попытку с новым ключом: эта закрыта",
		"provider-rejected":     "сообщает об отказе провайдера и сам не повторяет",
		"file-empty":            "просит выбрать непустой файл",
		"file-type-unsupported": "перечисляет принятые типы файла",
	}
}

// downgrades — почему класс модуля в этом монолите соврал бы клиенту. Правило,
// понижающее класс до 500, без строки здесь — находка.
func downgrades() map[error]string {
	return map[error]string{
		payment.ErrInvalidMoney: "сумму и валюту считает сервер из каталога: клиенту чинить нечего, а 400 спрятал бы дефект сборки в Debug-лог",
	}
}

// TestTranslate_EveryRuleReachable — правило на sentinel, которую монолит наружу
// не отдаёт, мёртвое: оно учит клиента ответу, которого не бывает. Вебхук не
// в счёт: словаря продукта у него нет.
func TestTranslate_EveryRuleReachable(t *testing.T) {
	all := outcomes()
	for _, r := range rules() {
		reached := slices.ContainsFunc(all, func(o outcome) bool { return errors.Is(o.err, r.sentinel) })
		if !reached {
			t.Errorf("правило мёртвое: %q не выходит наружу ни одним путём из outcomes", r.sentinel)
		}
	}
}

// TestResponders_WebhookSkipsProductRules — у ответчика классом нет словаря:
// правило item-not-open совпадает с ошибкой хука глубоко под ErrUnavailable, и
// продуктовый ответчик отдал бы провайдеру 403 вместо 503.
func TestResponders_WebhookSkipsProductRules(t *testing.T) {
	hook := itemClosedInHook()

	status, slug := answerOf(t, newResponder(quiet), hook)
	require.Equal(t, http.StatusForbidden, status, "условие пробы: правило продукта совпадает с ошибкой хука")
	require.Equal(t, "item-not-open", slug)

	status, slug = answerOf(t, newClassResponder(quiet), hook)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "unavailable", slug)
}

// outcome — ошибка в том виде, в каком её отдаёт наружу путь монолита, и ответ.
type outcome struct {
	route  string
	err    error
	status int
	slug   string
}

// outcomes — всё, что ручки для людей могут отдать наружу. Ошибки завёрнуты так,
// как их заворачивает модуль на этом пути: класс берёт самая внешняя
// классифицированная ошибка цепочки, и голая sentinel проверяла бы другой ответ.
func outcomes() []outcome {
	down := errors.New("хранилище не отвечает")
	return []outcome{
		{"POST /register", fmt.Errorf("%w: empty after normalization", loginid.ErrInvalid), http.StatusBadRequest, "login-invalid"},
		{"POST /register", password.ErrTooShort, http.StatusBadRequest, "password-too-short"},
		{"POST /register", password.ErrTooLong, http.StatusBadRequest, "password-too-long"},
		{"POST /register", password.ErrTooWeak, http.StatusBadRequest, "password-too-weak"},
		{"POST /register", fmt.Errorf("%w: recipient: %w", loginid.ErrInvalid,
			fmt.Errorf("%w: address syntax is invalid", mail.ErrInvalidMessage)), http.StatusBadRequest, "login-invalid"},
		{"POST /register, /signin", session.ErrTooManyAttempts, http.StatusTooManyRequests, "too-many-requests"},
		{"auth: все ручки", sessionFailed("lookup identity", down), http.StatusServiceUnavailable, "unavailable"},
		{"GET /confirm", session.ErrTokenInvalid, http.StatusBadRequest, "token-invalid"},
		{"POST /signin", session.ErrInvalidCredentials, http.StatusUnauthorized, "invalid-credentials"},
		{"POST /signin", session.ErrNotVerified, http.StatusForbidden, "email-not-verified"},
		{"ручки с сессией", session.ErrNoSession, http.StatusUnauthorized, "unauthenticated"},
		{"ручки с сессией", authhttp.ErrCSRF, http.StatusForbidden, "forbidden"},
		{"ручки с правами", fmt.Errorf("%w: role source: %w", authz.ErrUnavailable, down), http.StatusServiceUnavailable, "unavailable"},
		{"GET /lesson", fmt.Errorf("%w: policy: %w", authz.ErrUnavailable,
			fmt.Errorf("%w: %w", entitlement.ErrUnavailable, down)), http.StatusServiceUnavailable, "unavailable"},
		{"ручки с правами", denialOf(authz.Decision{Reason: authz.ReasonNoPermission}), http.StatusForbidden, "forbidden"},
		{"GET /lesson", denialOf(authz.Decision{Reason: authz.ReasonPolicy}), http.StatusForbidden, "item-not-open"},
		{"POST /checkout", fmt.Errorf("%w: key is empty", payment.ErrIdempotencyKeyInvalid), http.StatusBadRequest, "incorrect-input"},
		{"POST /checkout", fmt.Errorf("%w: key already describes intent", payment.ErrIdempotencyKeyReused), http.StatusConflict, "conflict"},
		{"POST /checkout", fmt.Errorf("%w: intent is expired", payment.ErrIntentClosed), http.StatusConflict, "payment-closed"},
		{"POST /checkout", fmt.Errorf("%w: intent", payment.ErrProviderRejected), http.StatusConflict, "provider-rejected"},
		{"POST /checkout", fmt.Errorf("create payment: %w", payment.ErrUnsupported), http.StatusNotImplemented, "not-implemented"},
		{"POST /checkout", fmt.Errorf("%w: create payment: %w", payment.ErrUnavailable, down), http.StatusServiceUnavailable, "unavailable"},
		// Сумму и состав строит код из каталога: оба отказа — дефект сборки, а не ввод.
		{"POST /checkout", fmt.Errorf("%w: outside the cap", payment.ErrInvalidMoney), http.StatusInternalServerError, httperr.DefaultInternalSlug},
		{"POST /checkout", fmt.Errorf("%w: items add up to less", payment.ErrInvalidRequest), http.StatusInternalServerError, httperr.DefaultInternalSlug},
		{"POST /upload", fmt.Errorf("%w: body is over the limit", objectstore.ErrTooLarge), http.StatusRequestEntityTooLarge, "payload-too-large"},
		{"POST /upload", objectstore.ErrEmptyBody, http.StatusBadRequest, "file-empty"},
		{"POST /upload", fmt.Errorf("%w: text/plain", objectstore.ErrUnsupportedType), http.StatusBadRequest, "file-type-unsupported"},
		{"POST /upload", objectstore.ErrSVGRejected, http.StatusBadRequest, "file-type-unsupported"},
		{"POST /upload", fmt.Errorf("%w: put", objectstore.ErrUnavailable), http.StatusServiceUnavailable, "unavailable"},
	}
}

// machineOutcomes — что отдаёт вебхук: ответчик классом, словаря продукта нет.
func machineOutcomes() []outcome {
	return []outcome{
		{"POST /webhook", payment.ErrInvalidSignature, http.StatusBadRequest, "incorrect-input"},
		{"POST /webhook", fmt.Errorf("%w: event has no provider event id", payment.ErrMalformedEvent), http.StatusBadRequest, "incorrect-input"},
		{"POST /webhook", fmt.Errorf("%w: apply event: %w", payment.ErrUnavailable, shoppg.ErrUnknownOrder), http.StatusServiceUnavailable, "unavailable"},
		// SlugError хука под недоступностью ядра — 503, а не её 409 (kit v0.3.0).
		{"POST /webhook", fmt.Errorf("%w: apply event: %w", payment.ErrUnavailable, errs.Conflict("seat-taken")), http.StatusServiceUnavailable, "unavailable"},
		// Sentinel с правилом продукта под недоступностью ядра — всё равно 503.
		{"POST /webhook", itemClosedInHook(), http.StatusServiceUnavailable, "unavailable"},
	}
}

// itemClosedInHook — ошибка хука зачисления, с которой совпадает правило item-not-open.
func itemClosedInHook() error {
	return fmt.Errorf("%w: apply event: %w", payment.ErrUnavailable,
		fmt.Errorf("%w: предмет снят с продажи", ErrItemNotOpen))
}

// sessionFailed — сбой, завёрнутый так же, как его заворачивает session.
func sessionFailed(op string, cause error) error {
	return fmt.Errorf("%w: session: %s: %w", auth.ErrUnavailable, op, cause)
}

// answerOf — статус и слаг, которыми ответчик монолита отвечает на err.
func answerOf(t *testing.T, respond *httperr.Responder, err error) (status int, slug string) {
	t.Helper()
	rec := httptest.NewRecorder()
	respond.Write(t.Context(), rec, err)
	var body struct {
		Slug string `json:"slug"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "тело ответа: %s", rec.Body.String())
	return rec.Code, body.Slug
}
