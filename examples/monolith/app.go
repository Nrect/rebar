package monolith

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
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
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/kit/errs/httperr"
	"github.com/nrect/rebar/kit/reqid"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/objectstore"
	objfs "github.com/nrect/rebar/objectstore/fs"
	"github.com/nrect/rebar/otelboot"
	"github.com/nrect/rebar/otelboot/errtrack"
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
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	obs   otelboot.Providers
	flush func(context.Context) error
	db    *shoppg.DB

	sessions *session.Service
	cookies  authhttp.CookieConfig
	guard    *authz.Authorizer
	roles    *authzpg.Store
	rights   *entitlement.Service
	journal  *audit.Recorder

	letters  *mail.Service
	producer *outbox.Producer
	worker   *outbox.Worker

	pay       *payment.Service
	reconcile *payment.Reconciler
	provider  *paymenttest.MemProvider

	uploader  *objectstore.Uploader
	collector *objectstore.Collector

	orders  *shoppg.Orders
	uploads *shoppg.Uploads

	gauges       gauges
	jobs         *scheduler.Scheduler
	schemaChecks []func(context.Context) error
	respond      *httperr.Responder
	respondClass *httperr.Responder
	handler      http.Handler
	probes       http.Handler

	// Процесс: порты и готовность заводит Start, гасит Stop.
	ready    atomic.Bool
	public   *http.Server
	internal *http.Server
	served   chan error
	stopOnce sync.Once
	stopErr  error
}

// New собирает приложение и накатывает миграции. Любая негодная часть
// конфигурации роняет сборку здесь, а не на первом запросе (CONVENTIONS §2).
// log — логгер процесса: блоки сами не логируют, запись ошибок им передают.
func New(ctx context.Context, cfg Config, log *slog.Logger, migrations fs.FS) (*App, error) {
	if log == nil {
		panic("monolith.New: nil log")
	}
	// Часы приложения: ручки берут момент отсюда, а не из time.Now (docs/CONSUMER.md, §8).
	a := &App{cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}
	if err := a.startInfra(ctx, migrations); err != nil {
		return nil, err
	}
	if err := a.startQueues(); err != nil {
		return nil, err
	}
	a.startIdentity()
	if err := a.startMoney(); err != nil {
		return nil, err
	}
	// Сверка — до задач и HTTP, и тем же списком, что у наката и /readyz.
	if err := checkAll(ctx, a.schemaChecks); err != nil {
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
	a.routes()
	return a, nil
}

// SetClock подменяет часы приложения и отдаёт ту же функцию каждому блоку со
// своими часами (docs/CONSUMER.md, §8, п. 1); адаптеры берут момент параметром.
// Только для тестов: до Start и до первого запроса. nil и вызов после Start —
// паника здесь, а не гонка с задачами.
func (a *App) SetClock(now func() time.Time) {
	if now == nil {
		panic("monolith.App.SetClock: now must not be nil")
	}
	// Порты заводит Start, и задачи после него уже читают часы.
	if a.public != nil {
		panic("monolith.App.SetClock: called after Start")
	}
	a.now = now
	a.sessions.SetClock(now)
	a.letters.SetClock(now)
	a.producer.SetClock(now)
	a.worker.SetClock(now)
	a.journal.SetClock(now)
	a.rights.SetClock(now)
	a.roles.SetClock(now)
	a.pay.SetClock(now)
	a.collector.SetClock(now)
	a.jobs.SetClock(now)
}

// startInfra — наблюдаемость, трекер и postgres: то, на чём стоит всё
// остальное. Наблюдаемость раньше пула: декораторы берут meter при сборке.
func (a *App) startInfra(ctx context.Context, migrations fs.FS) error {
	obs, err := otelboot.Start(ctx, otelboot.Config{
		ServiceName: "monolith", Version: a.cfg.Version, Commit: a.cfg.Commit,
		Environment: a.cfg.Environment, TracesEndpoint: a.cfg.TracesEndpoint, RuntimeMetrics: true,
	})
	if err != nil {
		return err
	}
	a.obs = obs
	if a.flush, err = errtrack.Init(a.cfg.SentryDSN.Reveal(), a.cfg.Environment, a.cfg.Version); err != nil {
		return err
	}
	db, err := shoppg.Open(ctx, a.cfg.DSN.Reveal(), postgres.Config{
		LockTimeout: 3 * time.Second, StatementTimeout: 10 * time.Second,
		MaxAttempts: 3, RetryBase: 20 * time.Millisecond,
	})
	if err != nil {
		return err
	}
	a.db = db
	blocks := schemaBlocks(db)
	if _, migErr := shoppg.Migrate(ctx, db.Pool, catalogs(blocks, migrations)); migErr != nil {
		return migErr
	}
	a.schemaChecks = schemaChecks(blocks)
	a.orders = shoppg.NewOrders(db)
	a.uploads = shoppg.NewUploads(db)
	return nil
}

// Close гасит собранное, но не запущенное приложение: наблюдаемость, пул и
// трекер. Идемпотентен. Запущенное гасит Stop.
func (a *App) Close(ctx context.Context) error {
	err := a.obs.Shutdown(ctx)
	a.db.Close()
	return errors.Join(err, a.flush(ctx))
}

// Handler — публичный HTTP-обработчик приложения.
func (a *App) Handler() http.Handler { return a.handler }

// Probes — служебные ручки: /metrics, /healthz, /readyz. Место им — на
// INTERNAL_ADDR, а не на публичном порту (probes.go).
func (a *App) Probes() http.Handler { return a.probes }

// CookieNames — имена сессионной и CSRF-куки. Нужны тому, кто ходит в
// приложение программно: имена зависят от того, есть ли TLS.
func (a *App) CookieNames() (sessionCookie, csrfCookie string) {
	return a.cookies.Name, a.cookies.CSRFName
}

// Transport — имя транспорта, с которым собрана почта. Имя, а не тип:
// декоратор метрик пробрасывает Name(), тип — нет, и Deliver узнаёт
// Unconfigured именно по имени (mail/unconfigured.go).
func (a *App) Transport() mail.TransportName { return a.letters.Transport() }

// readHeaderTimeout — срок заголовков запроса: медленный клиент не держит
// соединение бесконечно.
const readHeaderTimeout = 5 * time.Second

// Start занимает порты, снимает первый снимок гейджей, запускает задачи и
// объявляет готовность — в этом порядке (docs/CONSUMER.md, §4). Занятый порт —
// ошибка Start, а не горутины после того, как /readyz ответил 200.
//
// ctx — сигнальный: его отмена начинает остановку в Wait, а в задачи не
// доходит. Остановленный Stop процесс заново не стартует.
func (a *App) Start(ctx context.Context) error {
	public := &http.Server{Addr: a.cfg.Addr, Handler: a.handler, ReadHeaderTimeout: readHeaderTimeout}
	internal := &http.Server{Addr: a.cfg.InternalAddr, Handler: a.probes, ReadHeaderTimeout: readHeaderTimeout}
	lns, err := listen(ctx, public, internal)
	if err != nil {
		return err
	}
	// Адрес — занятого порта: при :0 номер выбирает система.
	public.Addr, internal.Addr = lns[0].Addr().String(), lns[1].Addr().String()
	a.public, a.internal = public, internal

	// ОТМЕНА КОНТЕКСТА РЕЖЕТ ПРОГОН ПОСРЕДИ РАБОТЫ: у mail это письмо посреди
	// отправки. Задачи гасит Stop между прогонами, а не сигнал.
	jobsCtx := context.WithoutCancel(ctx)
	// Первый снимок — до расписания: без него первую минуту после деплоя
	// payment_drift отдаёт ноль, а денежный алерт слеп. Сбой снимка уже записал
	// наблюдатель, и задача повторит его на своём такте.
	_, _ = a.jobs.RunNow(jobsCtx, jobGaugesSnapshot)
	a.jobs.Start(jobsCtx)

	a.served = make(chan error, len(lns))
	go func() { a.served <- public.Serve(lns[0]) }()
	go func() { a.served <- internal.Serve(lns[1]) }()
	a.ready.Store(true)
	a.log.InfoContext(ctx, "started", slog.String("op", "start"))
	return nil
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
	a.letters = mail.NewService(mailpg.New(a.db.Pool), transport, nil, mailConfig(a.cfg))

	queue := outboxpg.New(a.db.Pool)
	cfg := outboxConfig()
	a.producer = outbox.NewProducer(queue, cfg)

	worker, err := a.newWorker(queue, cfg)
	if err != nil {
		return err
	}
	a.worker = worker
	return nil
}

// outboxConfig — политика очереди событий сборки.
func outboxConfig() outbox.Config {
	return outbox.Config{
		Kinds:       outboxKinds(),
		MaxAttempts: 5,
		Backoff:     outbox.Backoff{Base: time.Second, Max: time.Minute},
		Lease:       30 * time.Second,
		// HandlerTimeout и BatchSize подобраны под jobsGrace: обычная пачка и два
		// HandlerTimeout на запись исхода и возврат остатка укладываются в бюджет
		// остановки (TestStopBudgets_FitKillDeadline).
		HandlerTimeout:  5 * time.Second,
		BatchSize:       20,
		Retention:       7 * 24 * time.Hour,
		MaxPayloadBytes: 16 * 1024,
	}
}

// mailConfig — политика почты сборки; её же берёт тест совпадения Recipients с
// письмом.
func mailConfig(cfg Config) mail.Config {
	return mail.Config{
		From:            mail.Address{Email: cfg.MailFrom, Name: "Магазин"},
		Kinds:           mailKinds(),
		MessageIDDomain: cfg.MailDomain,
		MaxAttempts:     5,
		Backoff:         mail.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           30 * time.Second,
		// SendTimeout и BatchSize подобраны под jobsGrace: обычная пачка и два
		// SendTimeout на запись исхода и возврат остатка укладываются в бюджет
		// остановки (TestStopBudgets_FitKillDeadline).
		SendTimeout:  5 * time.Second,
		BatchSize:    10,
		MinSendGap:   time.Millisecond,
		Retention:    7 * 24 * time.Hour,
		MaxBodyBytes: 64 * 1024,
		// Выбран с учётом остановки: процесс, убитый после jobsGrace, оставит под
		// арендой взятую пачку, и после Lease она уйдёт сама; дубль — только у
		// письма, которое отправлялось в момент убийства (docs/CONSUMER.md, §5).
		Uncertain: mail.UncertainRetry,
	}
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
		Recipients: recipients{},
		Auditor:    auditor{rec: a.journal},
	}, sessionConfig(a.cfg))

	a.cookies = cookieConfig(a.cfg)

	a.rights = entitlement.New(entitlementpg.New(a.db.Pool), entitlement.Config{
		TTL: a.cfg.EntitlementTTL, MaxSubjects: 10_000, LoadTimeout: 5 * time.Second,
	})
	// Реестр операций пустой: маршруты примера проверяют разрешение прямо, а
	// не по имени операции. Пустой — это NewRegistry(cfg, nil), а не nil:
	// nil-реестр это забытая проводка, и New на нём паникует.
	rights := authzConfig()
	a.roles = authzpg.New(a.db.Pool)
	a.guard = authz.New(a.roles, rights, a.entitlementPolicy(), authz.NewRegistry(rights, nil))
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

// routes — единственная точка сборки обработчиков: публичного и служебного.
func (a *App) routes() {
	a.respond = newResponder(a.log)
	a.respondClass = newClassResponder(a.log)
	mux := http.NewServeMux()
	a.mount(mux)
	// reqid снаружи всего: идентификатор запроса нужен и ответчику ошибок, и
	// журналу.
	a.handler = reqid.Middleware(mux)
	a.probes = a.probeMux()
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
