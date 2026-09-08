package authzhttp

import (
	"fmt"
	"net/http"

	"github.com/nrect/rebar/authz"
)

// Subjects — откуда взять субъекта запроса. Реализует потребитель: субъект
// кладёт в контекст тот, кто его аутентифицировал. Отсутствие субъекта —
// нулевой authz.Subject, а не паника: аноним это штатный случай.
type Subjects func(r *http.Request) authz.Subject

// Deny — как потребитель отвечает на отказ. Зовётся вместо хендлера; err
// ненулев только когда решение не принято (см. doc.go, отображение статусов).
type Deny func(w http.ResponseWriter, r *http.Request, d authz.Decision, err error)

// Config — проводка middleware. Нулевые поля — паника конструктора.
type Config struct {
	Authorizer *authz.Authorizer
	Subject    Subjects
	Deny       Deny
}

// Guard — фабрика middleware. Потокобезопасен: после New не меняется.
type Guard struct {
	authorizer *authz.Authorizer
	subject    Subjects
	deny       Deny
}

// New паникует на нулевых полях: отсутствующий Deny означал бы отказ без
// ответа, отсутствующий Subject — «все анонимы».
func New(cfg Config) *Guard {
	if cfg.Authorizer == nil {
		panic("authzhttp.New: Config.Authorizer must not be nil")
	}
	if cfg.Subject == nil {
		panic("authzhttp.New: Config.Subject must not be nil")
	}
	if cfg.Deny == nil {
		panic("authzhttp.New: Config.Deny must not be nil")
	}
	return &Guard{authorizer: cfg.Authorizer, subject: cfg.Subject, deny: cfg.Deny}
}

// Subject — субъект запроса так, как его видит middleware. Хендлеру он нужен
// для проверок по ресурсу (Authorizer.CanOn), которые маршруту не видны.
func (g *Guard) Subject(r *http.Request) authz.Subject { return g.subject(r) }

// Require — middleware на разрешение. Паникует на необъявленном разрешении:
// маршруты собираются на старте, и опечатка в константе обязана падать там же,
// а не превращаться в 403 на каждом запросе.
func (g *Guard) Require(p authz.Permission) func(http.Handler) http.Handler {
	if !g.authorizer.Knows(p) {
		panic(fmt.Sprintf("authzhttp.Require: permission %q is not declared in Config.Permissions", p))
	}
	return g.wrap(func(r *http.Request) (authz.Decision, error) {
		return g.authorizer.Can(r.Context(), g.subject(r), p)
	})
}

// RequireOp — middleware на операцию реестра. Паникует на неклассифицированной
// операции: молча отказывать на каждом запросе к маршруту хуже, чем не
// собраться, — второе видно на выкате, первое находится жалобой пользователя.
func (g *Guard) RequireOp(op authz.Operation) func(http.Handler) http.Handler {
	if _, ok := g.authorizer.Registry().Rule(op); !ok {
		panic(fmt.Sprintf("authzhttp.RequireOp: operation %q has no rule in the registry", op))
	}
	return g.wrap(func(r *http.Request) (authz.Decision, error) {
		return g.authorizer.CanOp(r.Context(), g.subject(r), op)
	})
}

// wrap — общая обвязка: решение до хендлера, отказ вместо него.
//
// ХЕНДЛЕР НЕ ЗОВЁТСЯ НИ ПРИ ОТКАЗЕ, НИ ПРИ СБОЕ. Пропуск «на всякий случай»
// при недоступном источнике ролей отменил бы весь пакет.
func (g *Guard) wrap(check func(r *http.Request) (authz.Decision, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := check(r)
			if err != nil || !d.Allowed {
				g.deny(w, r, d, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
