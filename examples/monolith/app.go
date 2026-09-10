package monolith

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/kit/errs/httperr"
	"github.com/nrect/rebar/kit/reqid"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/objectstore"
	objfs "github.com/nrect/rebar/objectstore/fs"
	"github.com/nrect/rebar/otelboot"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/scheduler"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// App — приложение целиком: все двенадцать модулей тулкита, сведённые в один
// процесс.
//
// ПОРЯДОК СБОРКИ НЕ СОВПАДАЕТ С ПОРЯДКОМ ЗАВИСИМОСТЕЙ МОДУЛЕЙ. mail и outbox
// собираются РАНЬШЕ auth и payment, потому что порты, которые реализует
// потребитель, тянут их к себе: session.Tokens кладёт письмо тем же коммитом,
// что и токен, а хук зачисления — событие тем же коммитом, что и книгу.
// Снизу вверх здесь: postgres → otelboot → mail → outbox → auth → authz →
// entitlement → payment → objectstore → scheduler.
type App struct {
	cfg Config
	obs otelboot.Providers
	db  *shoppg.DB

	sessions *session.Service
	cookies  authhttp.CookieConfig
	guard    *authz.Authorizer
	rights   *entitlement.Service
	journal  *audit.Recorder

	letters  *mail.Service
	producer *outbox.Producer
	worker   *outbox.Worker

	pay       *payment.Service
	payStore  payment.Store
	reconcile *payment.Reconciler
	provider  *paymenttest.MemProvider

	uploader  *objectstore.Uploader
	collector *objectstore.Collector

	orders  *shoppg.Orders
	grants  *shoppg.Entitlements
	uploads *shoppg.Uploads

	gauges  gauges
	jobs    *scheduler.Scheduler
	respond *httperr.Responder
	handler http.Handler
}

// New собирает приложение и накатывает миграции. Любая негодная часть
// конфигурации роняет сборку здесь, а не на первом запросе (CONVENTIONS §2).
func New(ctx context.Context, cfg Config, migrations fs.FS) (*App, error) {
	a := &App{cfg: cfg}
	if err := a.startInfra(ctx, migrations); err != nil {
		return nil, err
	}
	if err := a.startQueues(); err != nil {
		return nil, err
	}
	a.startIdentity()
	if err := a.startMoney(ctx); err != nil {
		return nil, err
	}
	if err := a.checkSchemas(ctx); err != nil {
		return nil, err
	}
	if err := a.startFiles(); err != nil {
		return nil, err
	}
	if err := a.startGauges(); err != nil {
		return nil, err
	}
	if err := a.startJobs(); err != nil {
		return nil, err
	}
	a.handler = a.routes()
	return a, nil
}

// startInfra — postgres и otelboot: то, на чём стоит всё остальное.
func (a *App) startInfra(ctx context.Context, migrations fs.FS) error {
	db, err := shoppg.Open(ctx, a.cfg.DSN.Reveal(), postgres.Config{
		LockTimeout: 3 * time.Second, StatementTimeout: 10 * time.Second,
		MaxAttempts: 3, RetryBase: 20 * time.Millisecond,
	})
	if err != nil {
		return err
	}
	a.db = db
	if migErr := shoppg.Migrate(ctx, db.Pool, migrations); migErr != nil {
		return migErr
	}
	obs, err := otelboot.Start(ctx, otelboot.Config{
		ServiceName: "monolith", Version: a.cfg.Version, Commit: a.cfg.Commit,
		Environment: "example", RuntimeMetrics: true,
	})
	if err != nil {
		return err
	}
	a.obs = obs
	a.orders = shoppg.NewOrders(db)
	a.grants = shoppg.NewEntitlements(db)
	a.uploads = shoppg.NewUploads(db)
	return nil
}

// Close гасит наблюдаемость и пул. Идемпотентен.
func (a *App) Close(ctx context.Context) error {
	err := a.obs.Shutdown(ctx)
	a.db.Close()
	return err
}

// Handler — HTTP-обработчик приложения.
func (a *App) Handler() http.Handler { return a.handler }

// CookieNames — имена сессионной и CSRF-куки. Нужны тому, кто ходит в
// приложение программно: имена зависят от того, есть ли TLS.
func (a *App) CookieNames() (sessionCookie, csrfCookie string) {
	return a.cookies.Name, a.cookies.CSRFName
}

// Jobs — планировщик фоновых задач.
func (a *App) Jobs() *scheduler.Scheduler { return a.jobs }

// Provider — двойник платёжного провайдера. Боевого адаптера в примере нет:
// его место — отдельный пакет, тянущий ровно одну библиотеку (payment/doc.go,
// «Чего в пакете НЕТ», п. 6).
func (a *App) Provider() *paymenttest.MemProvider { return a.provider }

// startQueues — mail и outbox. Собираются РАНЬШЕ auth и payment: их
// адаптеры уезжают внутрь чужих транзакций.
func (a *App) startQueues() error {
	transport, err := a.transport()
	if err != nil {
		return err
	}
	a.letters = mail.NewService(mailpg.New(a.db.Pool), transport, nil, mail.Config{
		From:            mail.Address{Email: a.cfg.MailFrom, Name: "Магазин"},
		Kinds:           mailKinds(),
		MessageIDDomain: a.cfg.MailDomain,
		MaxAttempts:     5,
		Backoff:         mail.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           30 * time.Second,
		SendTimeout:     10 * time.Second,
		BatchSize:       20,
		MinSendGap:      time.Millisecond,
		Retention:       7 * 24 * time.Hour,
		MaxBodyBytes:    64 * 1024,
		Uncertain:       mail.UncertainRetry,
	})

	queue := outboxpg.New(a.db.Pool)
	cfg := outbox.Config{
		Kinds:           outboxKinds(),
		MaxAttempts:     5,
		Backoff:         outbox.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           30 * time.Second,
		HandlerTimeout:  10 * time.Second,
		BatchSize:       20,
		Retention:       7 * 24 * time.Hour,
		MaxPayloadBytes: 16 * 1024,
	}
	a.producer = outbox.NewProducer(queue, cfg)

	worker, err := a.newWorker(queue, cfg)
	if err != nil {
		return err
	}
	a.worker = worker
	return nil
}

// startIdentity — auth, authz, entitlement и журнал.
func (a *App) startIdentity() {
	store := authpg.New(a.db.Pool)
	lt := letters{svc: a.letters, baseURL: a.cfg.BaseURL}
	tokens := shoppg.NewTokens(a.db, store.Tokens(), mailpg.New(a.db.Pool), lt.letterFor)

	a.journal = audit.NewRecorder(auditpg.New(a.db.Pool), audit.Config{
		Actions: auditActions(), MaxDetails: 8, MaxDetailLen: 256,
	})
	a.sessions = session.New(session.Deps{
		Identities: shoppg.NewIdentities(a.db),
		Sessions:   store,
		Attempts:   store,
		Tokens:     tokens,
		Hasher:     newHasher(),
		Policy:     newPolicy(),
		Notifier:   notifier{svc: a.letters, build: lt.message},
		Auditor:    auditor{rec: a.journal},
	}, sessionConfig(a.cfg))

	a.cookies = cookieConfig(a.cfg)

	a.rights = entitlement.New(a.grants, entitlement.Config{
		TTL: a.cfg.EntitlementTTL, MaxSubjects: 10_000, LoadTimeout: 5 * time.Second,
	})
	// Реестр операций пустой: маршруты примера проверяют разрешение прямо, а
	// не по имени операции. Пустой — это NewRegistry(cfg, nil), а не nil:
	// nil-реестр это забытая проводка, и New на нём паникует.
	rights := authzConfig()
	a.guard = authz.New(authzpg.New(a.db.Pool), rights, a.entitlementPolicy(),
		authz.NewRegistry(rights, nil))
}

// entitlementPolicy — штатная точка подключения второй оси: роль даёт
// разрешение «читать материал», а хук спрашивает у entitlement, куплен ли
// конкретный материал (authz/ports.go).
//
// Хук может ТОЛЬКО СУЗИТЬ решение, а его ошибка — недоступность, а не отказ.
func (a *App) entitlementPolicy() authz.Policy {
	return func(ctx context.Context, s authz.Subject, p authz.Permission,
		r authz.Resource,
	) (bool, error) {
		if p != permReadLesson || r.Zero() {
			return true, nil
		}
		subject, err := subjectUUID(s)
		if err != nil {
			return false, err
		}
		dec, err := a.rights.Allows(ctx, subject, r.ID)
		if err != nil {
			return false, err
		}
		return dec.Allowed, nil
	}
}

// sessionConfig — политика реалма. DefaultConfig это именованная рекомендация,
// а не умолчание: нулевой Config по-прежнему роняет New.
func sessionConfig(cfg Config) session.Config {
	c := session.DefaultConfig(cfg.Realm, cfg.Secret)
	// Неподтверждённых не пускаем: строгая регистрация — то, что проверяет
	// сквозной тест.
	c.AllowUnverifiedSignIn = false
	return c
}

// routes — единственная точка сборки обработчика.
func (a *App) routes() http.Handler {
	a.respond = httperr.New(httperr.Config{
		RequestID: reqid.From,
		Translate: translate,
	})
	mux := http.NewServeMux()
	a.mount(mux)
	// reqid снаружи всего: идентификатор запроса нужен и ответчику ошибок, и
	// журналу.
	return reqid.Middleware(mux)
}

// startFiles — objectstore: приём файлов и уборка сирот.
func (a *App) startFiles() error {
	root, err := absDir(a.cfg.FilesDir)
	if err != nil {
		return err
	}
	store := objfs.New(objfs.Config{Root: root, BaseURL: a.cfg.BaseURL + "/files"})
	a.uploader = objectstore.NewUploader(store, objectstore.UploaderConfig{
		Prefix:  "uploads",
		MaxSize: 5 << 20,
		Accept: []objectstore.ContentType{
			objectstore.ContentTypeJPEG, objectstore.ContentTypePNG, objectstore.ContentTypePDF,
		},
	})
	a.collector = objectstore.NewCollector(store, a.uploads, objectstore.CollectorConfig{
		Prefix: "uploads", MinAge: time.Hour, Mode: objectstore.CollectDryRun, BatchSize: 100,
	})
	return nil
}

// auditor — session.Auditor поверх audit.Recorder. Отдельный тип, потому что
// у портов разные словари: у session свой EventKind, у audit — свой Action.
type auditor struct{ rec *audit.Recorder }

// Record пишет событие входа в журнал безопасности.
//
// Актор кладётся В КОНТЕКСТ ЗДЕСЬ: audit.Recorder берёт его оттуда и отвечает
// ErrNoActor на забытую обвязку, а session про контекст аудита не знает.
func (w auditor) Record(ctx context.Context, ev session.Event) error {
	actor := audit.Actor{Kind: audit.ActorUser, ID: ev.SubjectID.String()}
	if ev.SubjectID == (auth.Principal{}).SubjectID {
		actor = audit.Actor{Kind: audit.ActorAnonymous}
	}
	return w.rec.Record(audit.NewContext(ctx, actor), audit.Entry{
		Action:  audit.Action("auth." + ev.Kind.String()),
		Outcome: auditOutcome(ev.Kind),
		Target:  audit.Target{Type: "identity", ID: ev.SubjectID.String()},
		IP:      ev.IP,
	})
}
