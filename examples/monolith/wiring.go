package monolith

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/pwzxcvbn"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailotel"
	"github.com/nrect/rebar/mail/smtp"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxotel"
)

// Разрешения витрины. Закрытый набор: проверка разрешения не из него — ошибка
// программиста, а не тихий отказ (authz/config.go).
const (
	permReadLesson authz.Permission = "lesson.read"
	permBuy        authz.Permission = "order.buy"
	permUpload     authz.Permission = "file.upload"
)

// Роли витрины.
const (
	roleCustomer authz.Role = "customer"
	roleStaff    authz.Role = "staff"
)

// authzConfig — модель прав. Персонал наследует покупателя: наследование
// задаётся ЯВНО, а не порядком объявления констант.
func authzConfig() authz.Config {
	return authz.Config{
		Permissions: []authz.Permission{permReadLesson, permBuy, permUpload},
		Roles: map[authz.Role][]authz.Permission{
			roleCustomer: {permReadLesson, permBuy, permUpload},
			roleStaff:    {},
		},
		Inherits: map[authz.Role][]authz.Role{roleStaff: {roleCustomer}},
	}
}

// cookieConfig — куки сессии.
//
// DefaultCookieConfig ЗДЕСЬ НЕ ГОДИТСЯ БЕЗ TLS: она даёт имена с префиксом
// __Host-, а он требует Secure, и на стенде по http конструктор паникует.
// Именованной «рекомендации для стенда» у пакета нет, поэтому конфигурация
// собирается руками (doc.go, «Что не сошлось»). Ослабляется ровно одно — Secure, и
// только там, где TLS нет вовсе.
func cookieConfig(cfg Config) authhttp.CookieConfig {
	if cfg.CookieSecure {
		return authhttp.DefaultCookieConfig(cfg.CookieName)
	}
	return authhttp.CookieConfig{
		Name:       cfg.CookieName,
		Secure:     false,
		SameSite:   http.SameSiteLaxMode,
		Path:       "/",
		CSRFName:   cfg.CookieName + "_csrf",
		CSRFHeader: authhttp.DefaultCSRFHeader,
	}
}

// subjectUUID — идентификатор субъекта authz как uuid. Негодная строка это
// НЕДОСТУПНОСТЬ, а не отказ: «не знаю, кто пришёл» и «этому нельзя» — разные
// ответы, и второй соврал бы клиенту.
func subjectUUID(s authz.Subject) (uuid.UUID, error) {
	id, err := uuid.Parse(s.ID)
	if err != nil {
		return uuid.Nil, errors.New("monolith: идентификатор субъекта не разобран")
	}
	return id, nil
}

// newHasher — argon2id по полам OWASP. Потолок одновременных хеширований —
// свойство памяти, а не частоты запросов, поэтому он здесь, а не в лимитере.
func newHasher() *password.Hasher {
	cfg := password.DefaultHasherConfig()
	return password.NewHasher(cfg)
}

// newPolicy — длина плюс оценка силы. Оценщик подключается портом: ядро auth
// zxcvbn не тащит.
func newPolicy() *password.Policy {
	return password.NewPolicy(pwzxcvbn.New(), password.DefaultPolicyConfig())
}

// notifier — session.Notifier: письма БЕЗ токена и без транзакции («на ваш
// адрес пытались зарегистрироваться», «пароль сменили»).
//
// Письма СО ССЫЛКОЙ сюда не попадают: они уходят через session.Tokens.Issue,
// потому что обязаны лечь одним коммитом со строкой токена.
type notifier struct {
	svc   *mail.Service
	build func(session.Notification) (mail.Message, bool)
}

// Notify кладёт письмо в очередь.
func (n notifier) Notify(ctx context.Context, note session.Notification) error {
	msg, ok := n.build(note)
	if !ok {
		return nil
	}
	_, err := n.svc.Enqueue(ctx, msg)
	return err
}

// auditActions — закрытый реестр действий журнала. Значение уезжает в метку
// метрики и в чужие алерты, поэтому набор объявлен, а не выведен из строк.
func auditActions() []audit.Action {
	out := make([]audit.Action, 0, len(session.AllEventKinds))
	for _, k := range session.AllEventKinds {
		out = append(out, audit.Action("auth."+k.String()))
	}
	return out
}

// auditOutcome — отказ и сбой разделены: всплеск denied это подбор пароля или
// ошибка в правах, всплеск failure — авария (audit/event.go).
func auditOutcome(kind session.EventKind) audit.Outcome {
	switch kind {
	case session.EventSignInFailed, session.EventLockedOut:
		return audit.OutcomeDenied
	default:
		return audit.OutcomeSuccess
	}
}

// transport — SMTP с метриками. Ошибка сборки транспорта не роняет
// приложение: письмо подождёт в очереди, а Unconfigured честно скажет, что
// провайдера нет.
func (a *App) transport() mail.Transport {
	var next mail.Transport = mail.Unconfigured{}
	if sender, err := smtp.New(a.cfg.SMTP); err == nil {
		next = sender
	}
	wrapped, err := mailotel.Wrap(next, a.obs.Meter.Meter("rebar.mail"))
	if err != nil {
		return next
	}
	return wrapped
}

// newWorker — воркер outbox с хендлерами под метриками и трейсом.
//
// Хендлер ОБЯЗАН БЫТЬ ИДЕМПОТЕНТНЫМ: доставка at-least-once, и Reclaimed
// говорит, что прошлая попытка могла оставить эффект (outbox/ports.go).
func (a *App) newWorker(store outbox.Store, cfg outbox.Config) *outbox.Worker {
	handlers, err := outboxotel.New(a.obs.Meter.Meter("rebar.outbox"),
		a.obs.Tracer.Tracer("rebar.outbox"), cfg)
	if err != nil {
		panic("monolith: инструменты outbox: " + err.Error())
	}
	handlers.Register(kindOrderPaid, outbox.HandlerFunc(a.onOrderPaid))
	handlers.Register(kindOrderRefunded, outbox.HandlerFunc(a.onOrderRefunded))

	worker, err := outbox.NewWorker(store, handlers.Registry(), cfg)
	if err != nil {
		panic("monolith: воркер outbox: " + err.Error())
	}
	return worker
}

// absDir — абсолютный путь каталога файлов: objectstore/fs требует именно его,
// и относительный путь означал бы «зависит от того, откуда запустили».
func absDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", err
	}
	return abs, nil
}
