package monolith

import (
	"errors"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/payment"
)

// ErrAccessDenied — отказ authz как ОШИБКА.
//
// Заведён здесь, потому что у пакета его нет: authz сообщает отказ полем
// Decision.Allowed и коллбэком Deny, а не sentinel-ошибкой, — в отличие от
// соседнего entitlement.ErrDenied. Единая таблица «ошибка → HTTP», которой
// живёт kit/errs/httperr, требует именно ошибки (doc.go, «Что не сошлось»).
var ErrAccessDenied = errors.New("monolith: access denied by authz")

// ErrItemNotOpen — отказ, пришедший ОТ ХУКА ПОЛИТИКИ, то есть от entitlement.
//
// Отличается от ErrAccessDenied только тем, что решение сузила вторая ось, а
// не роль. ПОЧЕМУ именно сузила — «купил и кончилось» или «не покупал» — сюда
// не доезжает: authz.Policy возвращает bool, и Reason самого entitlement
// теряется на границе (doc.go, «Что не сошлось»).
var ErrItemNotOpen = errors.New("monolith: item is not open to subject")

// denialOf — какой отказ отдать по решению authz.
//
// ReasonPolicy означает, что RBAC разрешил, а сузил хук: у нас это всегда
// entitlement. Остальные отказы — правило ролей.
func denialOf(d authz.Decision) error {
	if d.Reason == authz.ReasonPolicy {
		return ErrItemNotOpen
	}
	return ErrAccessDenied
}

// rule — «эта доменная ошибка отвечает этим слагом и этим классом».
type rule struct {
	sentinel error
	answer   errs.SlugError
}

// translate — ЕДИНСТВЕННАЯ точка перевода доменной ошибки в ответ.
//
// СБОЙ ХРАНИЛИЩА И ОТКАЗ В ПРАВАХ ОБЯЗАНЫ БЫТЬ РАЗЛИЧИМЫ У ПОТРЕБИТЕЛЯ: 503
// против 403. «Доступа нет» при упавшей базе — ложь клиенту и утопленный
// инцидент, потому что чинить будут права, а не базу (entitlement/doc.go, п. 2).
//
// Текст доменной ошибки в ответ НЕ УХОДИТ: наружу отдаётся только слаг из
// закрытого набора. В сообщениях Postgres лежат имена таблиц и ограничений, в
// ошибках auth — то, чего клиенту знать не следует.
func translate(err error) error {
	for _, r := range rules() {
		if errors.Is(err, r.sentinel) {
			return r.answer.WithCause(err)
		}
	}
	return err
}

// rules — таблица перевода. Порядок ЗНАЧИМ: недоступности стоят первыми,
// потому что часть отказов обёрнута в них (session.unavailable оборачивает
// auth.ErrUnavailable вокруг сбоя хранилища).
func rules() []rule {
	out := make([]rule, 0, 32)
	out = append(out, unavailableRules()...)
	out = append(out, authRules()...)
	out = append(out, paymentRules()...)
	out = append(out, uploadRules()...)
	return out
}

// unavailableRules — всё, что означает «ответа нет». Каждая даёт 503, а не 403
// и не 401.
func unavailableRules() []rule {
	return []rule{
		{auth.ErrUnavailable, errs.Unavailable("identity-store-unavailable")},
		{authz.ErrUnavailable, errs.Unavailable("authz-unavailable")},
		{entitlement.ErrUnavailable, errs.Unavailable("entitlement-unavailable")},
		{payment.ErrUnavailable, errs.Unavailable("payment-unavailable")},
		{mail.ErrUnavailable, errs.Unavailable("mail-unavailable")},
		{outbox.ErrUnavailable, errs.Unavailable("outbox-unavailable")},
		{objectstore.ErrUnavailable, errs.Unavailable("objectstore-unavailable")},
	}
}

// authRules — вход, регистрация и права.
func authRules() []rule {
	return []rule{
		{session.ErrInvalidCredentials, errs.Unauthenticated("invalid-credentials")},
		{session.ErrNoSession, errs.Unauthenticated("no-session")},
		{session.ErrNotVerified, errs.Forbidden("email-not-verified")},
		{session.ErrTooManyAttempts, errs.TooManyRequests("too-many-attempts")},
		{session.ErrTokenInvalid, errs.IncorrectInput("token-invalid")},
		{loginid.ErrInvalid, errs.IncorrectInput("login-invalid")},
		{password.ErrTooShort, errs.IncorrectInput("password-too-short")},
		{password.ErrTooLong, errs.IncorrectInput("password-too-long")},
		{password.ErrTooWeak, errs.IncorrectInput("password-too-weak")},
		{password.ErrBusy, errs.TooManyRequests("too-many-attempts")},
		// 403, а не 404: отказ по правилу — это отказ, и он обязан быть
		// отличим от «нет такой страницы» и от 503 выше.
		//
		// ErrAccessDenied — НАШ sentinel, а не пакетный: у authz его нет, он
		// сообщает отказ через Decision.Allowed (doc.go, «Что не сошлось»).
		{ErrAccessDenied, errs.Forbidden("access-denied")},
		{ErrItemNotOpen, errs.Forbidden("item-not-open")},
		{entitlement.ErrDenied, errs.Forbidden("item-not-open")},
		{auth.ErrLoginTaken, errs.Conflict("login-taken")},
	}
}

// paymentRules — оплата. Отказы клиента и конфликты, но не деньги.
func paymentRules() []rule {
	return []rule{
		{payment.ErrIdempotencyKeyInvalid, errs.IncorrectInput("idempotency-key-invalid")},
		{payment.ErrIdempotencyKeyReused, errs.Conflict("idempotency-key-reused")},
		{payment.ErrIdempotencyRace, errs.Conflict("idempotency-race")},
		{payment.ErrReferenceBusy, errs.Conflict("order-already-paying")},
		{payment.ErrIntentClosed, errs.Conflict("payment-closed")},
		{payment.ErrInvalidRequest, errs.IncorrectInput("purchase-invalid")},
		{payment.ErrInvalidMoney, errs.IncorrectInput("amount-invalid")},
		{payment.ErrUnknownIntent, errs.NotFound("payment-not-found")},
		{payment.ErrInvalidSignature, errs.IncorrectInput("webhook-not-authentic")},
		{payment.ErrMalformedEvent, errs.IncorrectInput("webhook-malformed")},
		{payment.ErrProviderRejected, errs.Conflict("provider-rejected")},
	}
}

// uploadRules — загрузка файлов.
func uploadRules() []rule {
	return []rule{
		{objectstore.ErrTooLarge, errs.PayloadTooLarge("file-too-large")},
		{objectstore.ErrEmptyBody, errs.IncorrectInput("file-empty")},
		{objectstore.ErrUnsupportedType, errs.IncorrectInput("file-type-unsupported")},
		{objectstore.ErrSVGRejected, errs.IncorrectInput("file-type-unsupported")},
		{objectstore.ErrNotFound, errs.NotFound("file-not-found")},
	}
}

// allSlugs — реестр слагов ответа, БЕЗ ПОВТОРОВ.
//
// Повторы законны и намеренны: у «пароль занят хешером» и «счётчик попыток
// исчерпан» один ответ клиенту, потому что различать их снаружи — значит
// рассказывать про устройство сервиса. Реестр же — это множество РАЗНЫХ
// ответов, и его держит errstest.CheckSlugRegistry.
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
