package authhttp

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

// ErrCSRF — небезопасный метод пришёл без совпадающего CSRF-токена. Отдельная
// ошибка, потому что это 403, а не 401: сессия у клиента есть, а доказательства
// того, что запрос отправил он сам, — нет.
var ErrCSRF = errors.New("csrf token mismatch")

// Resolver — то, чем middleware разбирает куку. *session.Service подходит как
// есть; порт объявлен здесь, чтобы тест потребителя мог подставить свой.
type Resolver interface {
	Resolve(ctx context.Context, rawToken string) (auth.Principal, error)
}

// Deny — как потребитель пишет отказ.
//
// ФОРМАТ ОШИБКИ ПРИНАДЛЕЖИТ ЕГО API, А НЕ ПАКЕТУ: у него уже есть один
// ответчик (kit/httperr), и второй, зашитый в middleware, дал бы клиенту два
// разных тела ошибки на соседних ручках. Пакет называет ПРИЧИНУ — сессии нет
// (session.ErrNoSession, 401), CSRF не сошёлся (ErrCSRF, 403), хранилище
// недоступно (auth.ErrUnavailable, 503), — статус выбирает потребитель.
type Deny func(w http.ResponseWriter, r *http.Request, err error)

type principalKey struct{}

// WithPrincipal кладёт принципала в контекст: middleware зовёт её сам, а тест
// потребителя — вместо middleware.
func WithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom достаёт принципала. Второе значение false означает «запрос без
// сессии»: пустой принципал наружу не выдаётся как настоящий.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	if !ok || p.IsZero() {
		return auth.Principal{}, false
	}
	return p, true
}

// Middleware требует живой сессии: без неё дальше по цепочке запрос не идёт.
// Публичные ручки просто не заворачиваются.
//
// ПОРЯДОК ПРОВЕРОК — ОТ ДЕШЁВОЙ К ДОРОГОЙ, и это не микрооптимизация. CSRF —
// сравнение двух строк, разбор сессии — круг в базу. Подделанный
// межсайтовый запрос несёт куку (в этом и состоит атака), но не несёт
// заголовка, поэтому проверка CSRF до Resolve отбивает его, не тратя
// соединение из пула.
func Middleware(res Resolver, cfg CookieConfig, deny Deny) func(http.Handler) http.Handler {
	mustWire(res, cfg, deny)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cfg.Name)
			if err != nil || cookie.Value == "" {
				deny(w, r, session.ErrNoSession)
				return
			}
			if !isSafeMethod(r.Method) && !csrfOK(r, cfg) {
				deny(w, r, ErrCSRF)
				return
			}
			principal, err := res.Resolve(r.Context(), cookie.Value)
			if err != nil {
				deny(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

func mustWire(res Resolver, cfg CookieConfig, deny Deny) {
	if res == nil {
		panic("authhttp.Middleware: resolver must not be nil")
	}
	if deny == nil {
		// Без Deny отказ пришлось бы писать самим — то есть завести второй
		// формат ошибки в API потребителя.
		panic("authhttp.Middleware: deny must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("authhttp.Middleware: " + err.Error())
	}
}

// isSafeMethod — методы, которые по RFC 9110 не меняют состояние. Всё
// остальное, включая нестандартные глаголы, считается небезопасным: список
// безопасных закрыт, список опасных — нет.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// csrfOK — double-submit: значение куки обязано совпасть со значением
// заголовка.
//
// СХЕМА СТОИТ НА ТОМ, ЧТО ЧУЖОЙ САЙТ НЕ МОЖЕТ ПРОЧИТАТЬ КУКУ, но может
// заставить браузер её отправить. Поставить СВОЮ куку с поддомена он, вообще
// говоря, может — и ровно поэтому имя с префиксом __Host- проверяется
// инвариантом CookieConfig: такую куку с поддомена не поставить.
func csrfOK(r *http.Request, cfg CookieConfig) bool {
	cookie, err := r.Cookie(cfg.CSRFName)
	if err != nil || cookie.Value == "" {
		return false
	}
	sent := r.Header.Get(cfg.CSRFHeader)
	if sent == "" {
		return false
	}
	return csrfMatches(cookie.Value, sent)
}

// csrfMatches — сравнение постоянного времени и ничего больше.
//
// ОТДЕЛЬНОЙ ФУНКЦИЕЙ РАДИ СТРАЖА: TestCSRF_ComparisonIsConstantTime читает
// её тело и требует subtle.ConstantTimeCompare. Обычное == вышло бы из цикла
// на первом несовпавшем байте, и токен подбирался бы по одному байту за
// раз — по времени ответа.
func csrfMatches(cookie, header string) bool {
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) == 1
}
