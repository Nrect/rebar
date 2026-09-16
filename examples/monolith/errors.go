package monolith

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
	"github.com/nrect/rebar/kit/reqid"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/payment"
)

// ErrItemNotOpen — отказ, пришедший ОТ ХУКА ПОЛИТИКИ, то есть от entitlement.
//
// Отличается от authz.ErrDenied только тем, что решение сузила ВТОРАЯ ОСЬ, а
// не роль. ПОЧЕМУ именно сузила — «купил и кончилось» или «не покупал» — сюда
// не доезжает: authz.Policy возвращает bool, и Reason самого entitlement
// теряется на границе. ОБХОД ПОСТОЯННЫЙ, не снимать при сходе портов
// (doc.go, «Что не сошлось: обходы постоянные», п. 1).
//
// Своей ошибки на ОБЫЧНЫЙ отказ здесь больше нет: её роль играет
// authz.ErrDenied. Эта осталась ровно потому, что различает ОСЬ, а не факт
// отказа, — а ось у authz.ErrDenied лежит в тексте, и разбирать текст ошибки
// хуже, чем сравнить типизированный Decision.Reason.
var ErrItemNotOpen = errors.New("monolith: item is not open to subject")

// denialOf — какой отказ отдать по решению authz.
//
// ReasonPolicy означает, что RBAC разрешил, а сузил хук: у нас это всегда
// entitlement. Остальные отказы — правило ролей.
func denialOf(d authz.Decision) error {
	if d.Reason == authz.ReasonPolicy {
		return ErrItemNotOpen
	}
	return fmt.Errorf("%w: %s", authz.ErrDenied, d.Reason)
}

// newResponder — ответчик ручек для людей: слаг продукта поверх класса.
// Ошибку ручки пишет он, в логгер процесса; своя запись рядом — дубль.
func newResponder(log *slog.Logger) *httperr.Responder {
	return httperr.New(httperr.Config{RequestID: reqid.From, Translate: translate, Logger: log})
}

// newClassResponder — ответчик ручек для машины (вебхук): класс без Translate.
// Машине слаги продукта не нужны, а правило, совпавшее с ошибкой хука глубоко
// под ErrUnavailable, превратило бы 503 в 4xx, и провайдер перестал бы повторять.
func newClassResponder(log *slog.Logger) *httperr.Responder {
	return httperr.New(httperr.Config{RequestID: reqid.From, Logger: log})
}

// rule — «эта доменная ошибка отвечает этим слагом и этим классом».
type rule struct {
	sentinel error
	answer   errs.SlugError
}

// translate — ЕДИНСТВЕННАЯ точка перевода доменной ошибки в слаг продукта.
//
// КЛАСС НЕСЁТ SENTINEL МОДУЛЯ (ADR-0007): без правила httperr отвечает статусом
// класса и слагом — именем класса, и 503 от упавшей базы не спутать с 403 от
// правил. Текст доменной ошибки в ответ НЕ УХОДИТ: в нём имена таблиц и то, чего
// клиенту знать не следует.
func translate(err error) error {
	for _, r := range rules() {
		if errors.Is(err, r.sentinel) {
			return r.answer.WithCause(err)
		}
	}
	return err
}

// rules — словарь продукта поверх классов модулей.
//
// СТРОКА ЕСТЬ, ТОЛЬКО ЕСЛИ БЕЗ НЕЁ ОТВЕТ ХУЖЕ: клиент по слагу поступит иначе,
// чем по имени класса (довод — clientActs в translate_test.go); sentinel решена
// //errs:nokind, а значение в этом монолите присылает клиент; либо класс модуля
// здесь соврал бы, и правило понижает его до 500 (довод — downgrades). Лишнюю,
// мёртвую и понижение без довода роняют TestTranslate_NoRedundantRule и
// TestTranslate_EveryRuleReachable.
func rules() []rule {
	out := make([]rule, 0, 16)
	out = append(out, authRules()...)
	out = append(out, paymentRules()...)
	out = append(out, uploadRules()...)
	return out
}

// authRules — вход, регистрация и права.
func authRules() []rule {
	return []rule{
		{session.ErrInvalidCredentials, errs.Unauthenticated("invalid-credentials")},
		{session.ErrNotVerified, errs.Forbidden("email-not-verified")},
		{session.ErrTokenInvalid, errs.IncorrectInput("token-invalid")},
		{loginid.ErrInvalid, errs.IncorrectInput("login-invalid")},
		{password.ErrTooShort, errs.IncorrectInput("password-too-short")},
		{password.ErrTooLong, errs.IncorrectInput("password-too-long")},
		{password.ErrTooWeak, errs.IncorrectInput("password-too-weak")},
		// Своя sentinel без класса: различает ось отказа, а не факт (doc.go,
		// «Что не сошлось: обходы постоянные», п. 1).
		{ErrItemNotOpen, errs.Forbidden("item-not-open")},
	}
}

// paymentRules — концы попытки оплаты, по которым клиент поступает по-разному, и
// сумма из каталога.
func paymentRules() []rule {
	return []rule{
		{payment.ErrIntentClosed, errs.Conflict("payment-closed")},
		{payment.ErrProviderRejected, errs.Conflict("provider-rejected")},
		// Сумму и валюту считает сервер: негодная — дефект сборки, а не ввод.
		{payment.ErrInvalidMoney, errs.Unknown(httperr.DefaultInternalSlug)},
	}
}

// uploadRules — загрузка файлов.
func uploadRules() []rule {
	typeUnsupported := errs.IncorrectInput("file-type-unsupported")
	return []rule{
		{objectstore.ErrEmptyBody, errs.IncorrectInput("file-empty")},
		{objectstore.ErrUnsupportedType, typeUnsupported},
		{objectstore.ErrSVGRejected, typeUnsupported},
	}
}

// allSlugs — реестр слагов ответа, БЕЗ ПОВТОРОВ.
//
// Повторы законны и намеренны: SVG и чужой тип — клиенту один ответ. Реестр же
// — это множество РАЗНЫХ ответов, и его держит errstest.CheckSlugRegistry.
func allSlugs() []string {
	seen := make(map[string]bool, 32)
	out := make([]string, 0, 32)
	for _, r := range rules() {
		slug, ok := errs.SlugOf(r.answer)
		if !ok {
			panic("monolith: правило без слага")
		}
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}
