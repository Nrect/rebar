# Контракт проекта-потребителя

[CONVENTIONS.md](../CONVENTIONS.md) говорит, как пишется блок. Здесь — как
пишется проект, который блоки собирает: логи, окружение, служебные ручки,
старт, остановка, миграции, корреляция. У блока нет `main`, сигнала, порта и
окружения, поэтому этот остаток принадлежит проекту; всё, что можно закрыть в
самом блоке, закрывается в блоке ([ADR-0010](adr/0010-block-owns-guarantees.md)).

Каждое правило записано одинаково: **правило**, почему, образец. Образец — файл
[`examples/monolith`](../examples/monolith) там, где пример уже делает так; где
не делает, приведён код. Код сверен с API блоков на 2026-09-16 и собирается;
`load`, `openPool` и `assemble` с его результатом `app` — код самого проекта.
Примеры собраны в один файл ради чтения: проект, взявший `.golangci.yml`
тулкита, держит pgx в каталоге `*pg`, а otel — в `*otel`, как монолит
(`shoppg`, `lockotel`).

| # | Раздел | Закрывает |
|---|---|---|
| 1 | [Логи](#1-логи) | секрет или адрес в логе; запись, которую не найти по `request_id` |
| 2 | [Переменные окружения](#2-переменные-окружения) | выкат, который чинят по одной переменной за цикл; предохранитель, снятый забытой переменной |
| 3 | [Служебные ручки](#3-служебные-ручки) | трафик в процесс, чьей схемы нет в базе; метрики наружу |
| 4 | [Порядок старта](#4-порядок-старта) | запрос и прогон на непроверенной схеме |
| 5 | [Порядок остановки](#5-порядок-остановки) | второе письмо или «не отправлено» у доставленного |
| 6 | [Миграции](#6-миграции) | ручной `ALTER` в каждом проекте |
| 7 | [Трассировка и корреляция](#7-трассировка-и-корреляция) | ответ об ошибке, по которому не найти причину |

## 1. Логи

**JSON из `log/slog` в stdout, установленный первой строкой `main`.** Логи
забирает со stdout агент выката: `otelboot` экспорта логов не делает намеренно,
второй канал — вторая точка отказа ([otelboot/doc.go](../otelboot/doc.go),
«Чего нет»). JSON — потому что ищут по ключам, а не регуляркой. Первой строкой —
потому что блок, которому не передали логгер, пишет в `slog.Default()`
(`httperr.Config.Logger`, `scheduler.LogObserver` и `payment.LogObserver` на
`nil`), и без `SetDefault` его записи, как и ошибка конфига, уходят текстом
стандартного `log`.

```go
func newLogger(level *slog.LevelVar) *slog.Logger {
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	tracked := errtrack.WrapLogger(slog.New(base)).Handler() // Error → трекер; без SENTRY_DSN — пустышка
	return slog.New(ctxHandler{next: tracked})               // идентификаторы из контекста, раздел 7
}
```

**Блок не логирует сам.** Он принимает `*slog.Logger` от проекта —
`httperr.Config.Logger`, `scheduler.LogObserver`, `payment.LogObserver`,
`audit.NewLogSink`, `errtrack.WrapLogger` — либо возвращает ошибку. Уровень,
приёмник и срок хранения — свойства выката, и блок, пишущий сам, решал бы за
проект, что Error, а что шум ([audit/doc.go](../audit/doc.go), п. 7). На приёмке
блока это проверяется ([CHIP](CHIP.md), критерий 9).

Два следствия для проекта:

- ошибку ручки пишет `httperr`, ошибку прогона — наблюдатель планировщика;
  своя запись рядом — вторая строка об одной поломке;
- `schedulerotel` пишет только метрики: прогон, упавший на недоступной базе,
  виден счётчиком без текста ошибки. Текст даёт `scheduler.LogObserver`, и оба
  подключаются одним наблюдателем.

```go
// observers — метрика для алерта и запись с текстом ошибки для разбора.
type observers []scheduler.Observer

func (o observers) Started(jobs []string, at time.Time) {
	for _, x := range o {
		x.Started(jobs, at)
	}
}

func (o observers) Finished(ctx context.Context, run scheduler.Run) {
	for _, x := range o {
		x.Finished(ctx, run)
	}
}
```

**Один словарь ключей на процесс.** Запрос «все записи этого запроса» или «все
сбои класса `unavailable`» пишется один раз и находит записи и блоков, и
проекта. Где блок уже пишет слово, словарь берёт его: переименование разломало
бы поиск по записям блока.

| Ключ | Что | Кто пишет |
|---|---|---|
| `request_id` | идентификатор запроса | обработчик из контекста (раздел 7); `httperr` — сам |
| `trace_id` | трасса W3C, 32 hex-символа; нет спана — нет ключа | обработчик из контекста |
| `subject_id` | UUID субъекта (`auth.Principal.SubjectID`), не логин | обработчик из контекста |
| `op` | операция: `r.Pattern` в обработчике (`POST /checkout`) или имя доменной операции | проект; `payment.LogObserver` |
| `job` | имя фоновой задачи | `scheduler.LogObserver` |
| `result` | `ok`, `rejected` или `error` — тот же словарь, что у метки метрики | проект |
| `error` | текст ошибки, `slog.Any("error", err)` | `httperr`, `scheduler.LogObserver`, проект |
| `error_kind` | класс, `errs.KindOf(err)` | проект |
| `slug` | слаг ответа | `httperr` |
| `status` | HTTP-статус | `httperr` |
| `took` | длительность, `slog.Duration`; JSON пишет её целым числом наносекунд | `scheduler.LogObserver`, проект |

- `request_id`, `trace_id` и `subject_id` вызывающий код не пишет: забытый ключ
  не находится никогда, а обработчик не забывает;
- `op` — шаблон маршрута, а не путь и не URL: в строке запроса бывает токен
  (`GET /confirm?token=` у монолита), а путь с идентификаторами плодит значения;
- `rejected` — отказ определённый, повтор бесполезен (4xx, отказ провайдера);
  `error` — ответа нет, повтор осмыслен (5xx, сбой). Сведи их — и алерт на сбой
  загорится от штатных отказов ([CONVENTIONS §6](../CONVENTIONS.md#6-наблюдаемость));
- `error_kind`, а не `kind`: `kind` у `mail` и `outbox` — вид письма и события;
- сообщение записи — постоянная строка без подстановок, по-английски в нижнем
  регистре, как у блоков (`http error`, `cron job failed`); переменное — в ключи;
- ключи плоские, `Logger.WithGroup` не используется: ключи из контекста уехали
  бы внутрь группы.

**Уровни — по правилу `httperr`, и свои записи следуют ему же.**

| Уровень | Когда | Пример |
|---|---|---|
| Error | нужен человек; запись уходит в трекер | 5xx, упавший прогон, отказ старта, исчерпанный бюджет остановки |
| Warn | сигнал безопасности или деградация, которая пройдёт сама | 401, 403, 429; `/readyz` ответил 503 |
| Info | жизненный цикл процесса | старт, начало остановки |
| Debug | штатный отказ и штатный успех | прочие 4xx, успешный прогон задачи |

Отменённый контекст запроса записи не даёт: клиент ушёл, сервер не сбоил. Сведи
уровни — и сканер портов зальёт Error, а алерт по нему перестанут читать
([httperr/doc.go](../kit/errs/httperr/doc.go), пп. 5–6). В проде уровень
`info`: штатные 4xx не пишутся, 401, 403 и 429 видны.

**Чего в логе нет никогда.**

| Что | Почему | Вместо |
|---|---|---|
| логин и адрес почты | персональные данные, а лог переживает инцидент дольше всех ([auth/loginid/doc.go](../auth/loginid/doc.go), п. 7) | `subject_id` |
| тело письма | в нём ссылка с токеном ([mail/doc.go](../mail/doc.go), п. 3) | идентификатор строки очереди |
| токены, секреты, DSN, куки, заголовок `Authorization` | записанный ключ — утёкший ключ | `config.Secret` и `token.Secret` редактируются в fmt, slog и JSON |
| ключ объекта хранилища и имя файла | ключ бывает выведен из персональных данных ([objectstore/doc.go](../objectstore/doc.go), п. 9) | идентификатор записи проекта |
| `Detail` ошибки Postgres | «Failing row contains (…)» — строка целиком: хэш пароля, тело письма ([postgres/doc.go](../postgres/doc.go), п. 1) | свой SQL проекта отдаёт ошибку через `postgres.Sanitize` |
| URL со строкой запроса, тела запросов и ответов, payload событий | там токены и персональные данные, которых блок не видит | `op`, `status` |

Образец границы своего SQL — `storeError` в
[`examples/monolith/shoppg/db.go`](../examples/monolith/shoppg/db.go). Журнал
аудита: `audit.LogSink` пишет `audit.actor_name` и `audit.ip`, поэтому в
`audit.Actor` проект кладёт идентификатор, а не логин — как `auditor` в
[`examples/monolith/app.go`](../examples/monolith/app.go).

## 2. Переменные окружения

**Имя — `UPPER_SNAKE_CASE`, подсистема — префиксом, имени проекта в ключе нет.**
`DATABASE_URL`, `SMTP_HOST`, `SESSION_COOKIE_SECURE`. Блоки стоят в нескольких
проектах, и одинаковые имена переносят шаблоны выката и инструкции без правки;
окружение процесса и так принадлежит одному сервису.

Имена, общие для всех проектов:

| Переменная | Что | Умолчание |
|---|---|---|
| `ENVIRONMENT` | `development`, `staging`, `production` или своё `[a-z0-9_-]` | нет: «правильного» окружения не существует, а пустая метка сливает прод и dev на дашборде (`otelboot.Config.Environment`) |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `ADDR` | адрес публичного HTTP | `:8080` |
| `INTERNAL_ADDR` | адрес служебного HTTP: `/metrics`, `/healthz`, `/readyz` | `127.0.0.1:9090` — наружу порт открывают явно |
| `DATABASE_URL` | DSN, секрет | нет |
| `OTEL_TRACES_ENDPOINT` | приёмник OTLP/HTTP | пусто — трейсинг выключен |
| `SENTRY_DSN` | трекер ошибок, секрет | пусто — трекер выключен |

**Всё окружение читает один `config.Loader`, и старт падает списком.** Читатели
не возвращают ошибку по одной, а копят; `Err()` отдаёт всё одним `errors.Join`,
строкой на ключ. Пять забытых переменных — один перезапуск, а не пять
([kit/config/doc.go](../kit/config/doc.go)). Значение в текст ошибки не
попадает, только имя ключа и ожидание, поэтому список печатается в лог выката
как есть. Перекрёстная проверка («`SMTP_AUTH=plain` требует `SMTP_PASSWORD`») —
`Loader.Fail(key, reason)`, в тот же список. Образец — `Load` в
[`examples/monolith/config.go`](../examples/monolith/config.go).

```go
func load(l *config.Loader) (Config, error) {
	cfg := Config{
		Environment:    l.Required("ENVIRONMENT"),
		LogLevel:       l.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		Addr:           l.Optional("ADDR", ":8080"),
		InternalAddr:   l.Optional("INTERNAL_ADDR", "127.0.0.1:9090"),
		DSN:            l.Secret("DATABASE_URL", 1),
		TracesEndpoint: l.Optional("OTEL_TRACES_ENDPOINT", ""),
		SentryDSN:      l.OptionalSecret("SENTRY_DSN", 1),
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
```

**Обязательна переменная, без которой процесс в проде неверен.** DSN, секреты,
`ENVIRONMENT`, учётные данные провайдера в режиме, который их требует.
Умолчание допустимо, только если оно безопасно для прода: порт, таймаут, лимит.
Умолчание, ослабляющее защиту, запрещено: `SESSION_COOKIE_SECURE` по умолчанию
`true`, `SMTP_ALLOW_PLAINTEXT` — `false`, а стенд ставит послабление явно.
Забытая переменная не должна молча снимать предохранитель — то же правило, что
«нулевое значение `Config` — отказ» ([CONVENTIONS §2](../CONVENTIONS.md#2-безопасность)).

**Режим — закрытый набор через `Loader.Enum`.** `SMTP_TRANSPORT=smtp|unconfigured`:
опечатка падает на старте, а «писем не шлём» получает только тот, кто выбрал это
словом. Образец — `TransportMode` в
[`examples/monolith/config.go`](../examples/monolith/config.go).

**У секрета нет умолчания.** `Loader.Secret(key, minLen)`: ключа нет или он
короче `minLen` — отказ; «забыли задать» не должно становиться предсказуемым
ключом из кода ([kit/config/doc.go](../kit/config/doc.go), п. 3). Секрет, не
нужный в этом режиме, читается `OptionalSecret`, а не `Optional`: строка без
типа печатается первым же `%v`. Значение достаёт только `Reveal()`, и зовут его
там, где секрет уходит в конфиг блока.

**Процесс читает только окружение.** `.env` и файлы подкладывает среда запуска
(`docker compose`, `direnv`), а не бинарь: разбор чужого формата — это и код, и
путь до диска ([kit/config/doc.go](../kit/config/doc.go), п. 6).

## 3. Служебные ручки

**Три ручки — на служебном порту `INTERNAL_ADDR`, ни одной на публичном роутере.**

| Ручка | Вопрос | Кто пишет | Что делает |
|---|---|---|---|
| `/metrics` | что с процессом в числах | `otelboot`: обработчик `Providers.Metrics` | в базу не ходит: гейджи обновляет отдельная задача ([PATTERNS §8](PATTERNS.md#8-наблюдаемость-декоратором), п. 4) |
| `/healthz` | жив ли процесс | проект | 200, никуда не ходит |
| `/readyz` | можно ли слать трафик | проект | до конца старта порт не слушается, с начала остановки — 503; в остальное время `CheckSchema` каждого подключённого блока |

Служебный порт — потому что `/metrics` отдаётся без авторизации и рассказывает
версию сборки, объём трафика и внутренние имена
([otelboot/doc.go](../otelboot/doc.go), п. 1), а `/readyz` на каждый вызов
делает около полусотни запросов к каталогу базы (семь блоков): на публичном
порту это усилитель нагрузки.

**`/healthz` в базу не ходит.** Провал liveness — перезапуск, а упавшую базу
перезапуск не чинит; процесс, который перезапускают по кругу, ещё и обрывает
прогоны задач. Образец тела ручки — `healthz` в
[`examples/monolith/handlers.go`](../examples/monolith/handlers.go).

**`/readyz` сверяет схему каждого блока тем же списком, что и старт.**
`CheckSchema(ctx) error` есть у всех семи адаптеров: `mailpg`, `outboxpg`,
`authpg`, `authzpg`, `paymentpg`, `entitlementpg` — у `*Store`, `auditpg` — у
`*Sink`. Недоступная база приходит классом `unavailable`, расхождение — ошибкой
без класса с подсказкой первой строкой; для пробы оба ответа — 503. Так выкат
версии, чьей схемы в базе нет, останавливается на новых репликах, а не на
запросах пользователей; колонку, добавленную миграцией вперёд, старый код
расхождением не считает, и старые реплики остаются готовыми. Список и функция
сверки одни на старт и на пробу (`checkAll`): сверка, добавленная только в одно
место, расходится молча.

**Ответ `/readyz` — только код; причина — в лог уровнем Warn.** Проба
повторяется каждые несколько секунд, и Error засыпал бы трекер одним и тем же
инцидентом; тексты расхождений называют таблицы и колонки — это оператору, а не
балансировщику. У пробы свой срок, период — не чаще раза в 10 с.

```go
// probes — служебные ручки. checks — тот же список, которым сверялся старт.
func probes(metrics http.Handler, checks []func(context.Context) error, ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := checkAll(ctx, checks); err != nil {
			slog.WarnContext(ctx, "not ready", slog.String("op", "readyz"), slog.Any("error", err))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// checkAll — сверка схемы всех блоков: одна на старт и на /readyz.
func checkAll(ctx context.Context, checks []func(context.Context) error) error {
	for _, check := range checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}
```

## 4. Порядок старта

**Логгер → конфиг → наблюдаемость → пул → блоки и `CheckSchema` → фоновые
задачи → HTTP.** Каждый шаг падает раньше, чем следующий успеет что-то принять
или записать.

1. **Логгер** — ошибка конфига тоже запись лога (раздел 1).
2. **Конфиг** — до всего, что открывает соединения и порты: негодное окружение
   падает списком, не оставив за собой ни пула, ни занятого порта.
3. **Наблюдаемость** — до сборки блоков: декораторы `<pkg>otel` берут meter при
   сборке (`mailotel.Wrap`, `schedulerotel.NewObserver`). `otelboot.Start`
   проверяет свой конфиг сам и отказывает на старте, а не оставляет трейсинг
   живым с виду и отправляющим в никуда ([otelboot/doc.go](../otelboot/doc.go), п. 6).
4. **Пул** — `postgres.WithUTC`, `pgxpool.New`, `Ping`. UTC-пин отвязывает даты
   на сервере от образа базы ([postgres/doc.go](../postgres/doc.go), п. 5), а
   недоступная база становится отказом старта, а не первого запроса. Текст DSN
   в ошибку не попадает: в нём пароль. Образец — `Open` в
   [`examples/monolith/shoppg/db.go`](../examples/monolith/shoppg/db.go). Если
   миграции накатывает сам процесс, их место здесь (раздел 6).
5. **Блоки и `CheckSchema`** — конструкторы блоков паникуют на негодном `Config`
   ([CONVENTIONS §2](../CONVENTIONS.md#2-безопасность)), `CheckSchema` находит
   расхождение и первой строкой говорит, что делать. До задач и HTTP — потому
   что первый `Claim` на такой схеме упал бы ошибкой SQL без подсказки, а
   первый запрос — у пользователя.
6. **Фоновые задачи** — `scheduler.New` отвергает дубль имени и непозитивный
   интервал ошибкой ([scheduler/doc.go](../scheduler/doc.go), пп. 4–5), и процесс
   с неверным набором задач не успевает стать готовым. Первый снимок гейджей —
   `RunNow` до `Start`: сам планировщик прогона при старте не делает (образец —
   `App.Start` в [`examples/monolith/app.go`](../examples/monolith/app.go)).
   Контекст задач сигналом не отменяется — раздел 5.
7. **HTTP** — порты занимаются до «готов» (`listen`): занятый порт — отказ
   старта, а не ошибка в горутине после того, как `/readyz` ответил 200.

```go
var version, commit string // -ldflags "-X main.version=… -X main.commit=…"

func main() {
	if err := run(); err != nil {
		slog.Error("exit", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	var level slog.LevelVar
	slog.SetDefault(newLogger(&level))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := load(config.FromEnv())
	if err != nil {
		return err
	}
	_ = level.UnmarshalText([]byte(cfg.LogLevel)) // значение уже из закрытого набора Enum

	p := process{ready: new(atomic.Bool)}
	p.obs, err = otelboot.Start(ctx, otelboot.Config{
		ServiceName: "shop", Version: version, Commit: commit,
		Environment: cfg.Environment, TracesEndpoint: cfg.TracesEndpoint, SetGlobals: true,
	})
	if err != nil {
		return err
	}
	if p.flush, err = errtrack.Init(cfg.SentryDSN.Reveal(), cfg.Environment, version); err != nil {
		return err
	}

	if p.pool, err = openPool(ctx, cfg.DSN); err != nil { // как shoppg.Open
		return err
	}
	app := assemble(cfg, p.pool, p.obs) // блоки; паника на негодном Config
	if schemaErr := checkAll(ctx, app.schemaChecks); schemaErr != nil {
		return schemaErr
	}

	if p.jobs, err = scheduler.New(app.jobObserver, app.jobs...); err != nil {
		return err
	}

	p.public = &http.Server{Addr: cfg.Addr, Handler: app.handler, ReadHeaderTimeout: 5 * time.Second}
	p.internal = &http.Server{Addr: cfg.InternalAddr, ReadHeaderTimeout: 5 * time.Second,
		Handler: probes(p.obs.Metrics, app.schemaChecks, p.ready)}
	lns, err := listen(ctx, p.public, p.internal)
	if err != nil {
		return err
	}

	p.jobs.Start(context.WithoutCancel(ctx)) // гасит Stop между прогонами, а не сигнал
	served := make(chan error, 2)
	go func() { served <- p.public.Serve(lns[0]) }()
	go func() { served <- p.internal.Serve(lns[1]) }()
	p.ready.Store(true)
	slog.Info("started", slog.String("op", "start"))

	select {
	case <-ctx.Done(): // сигнал
	case err = <-served: // сервер упал сам — останавливаемся тем же порядком
	}
	return errors.Join(err, p.stop(ctx)) // раздел 5
}

// listen занимает порты до «готов»: занятый порт — отказ старта.
func listen(ctx context.Context, servers ...*http.Server) ([]net.Listener, error) {
	var lc net.ListenConfig
	lns := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}
```

## 5. Порядок остановки

**Обратный старту: `/readyz` → 503, публичный `Shutdown`, `scheduler.Stop` с
бюджетом, пул, служебный порт и сброс телеметрии.**

1. **Трафик — первым.** Каждая секунда приёма после сигнала — запрос, который
   могут оборвать на `SIGKILL`. `http.Server.Shutdown` закрывает приём сразу и ждёт
   текущие запросы; заодно дописываются прогоны, запущенные из ручки через
   `RunNow`, — `scheduler.Stop` их не ждёт
   ([scheduler/scheduler.go](../scheduler/scheduler.go), `Stop`). Балансировщику,
   который узнаёт о выходе только по `/readyz`, между шагами нужна пауза не
   короче периода его опроса.
2. **Планировщик — между прогонами, а не отменой.** `Stop` снимает расписание и
   ждёт текущие прогоны ([scheduler/doc.go](../scheduler/doc.go), п. 2). Отмена
   контекста прогон не прерывает: её замечает сама задача — и замечает посреди
   работы. Поэтому `Start` получает контекст, который сигнал не отменяет:
   `jobs.Start(context.WithoutCancel(ctx))`.
3. **Пул — после запросов и задач.** Прогон, у которого закрыли пул, не запишет
   исход так же, как отменённый. `pgxpool.Close` ждёт, пока вернут все
   соединения, поэтому после исчерпанного бюджета пул не закрывают: остаток
   срока ушёл бы на ожидание, и процесс убили бы в нём.
4. **Служебный порт и телеметрия — последними.** `/healthz` отвечает, пока
   дописываются прогоны; `Providers.Shutdown` сбрасывает батч спанов — без него
   теряются трейсы последних секунд ([otelboot/doc.go](../otelboot/doc.go), п. 4).

**Бюджеты явные, и их сумма меньше срока, после которого процесс убивают:**
`docker stop` по умолчанию ждёт 10 с, Kubernetes — `terminationGracePeriodSeconds`,
30 с. Бюджет задач — не меньше обычного прогона: у `mail` это `BatchSize` писем
с паузой `MinSendGap`, у `outbox` — `BatchSize` хендлеров. Длинный прогон
укорачивают размером пачки, а не сроком внутри `Run`: срок, истёкший посреди
отправки или хендлера, оставляет под арендой именно эту строку (ниже). Цена
правила — `outbox` на остановке дописывает пачку хендлерами, а не возвращает её
в очередь (`releaseRest`); дубля это не даёт.

```go
// process — то, что старт отдаёт остановке.
type process struct {
	public, internal *http.Server
	ready            *atomic.Bool
	jobs             *scheduler.Scheduler
	pool             *pgxpool.Pool
	obs              otelboot.Providers
	flush            func(context.Context) error
}

const (
	httpGrace  = 10 * time.Second
	jobsGrace  = 15 * time.Second
	flushGrace = 3 * time.Second // сумма меньше срока, после которого процесс убивают
)

// stop — остановка в обратном старту порядке, у каждого шага свой бюджет.
func (p process) stop(ctx context.Context) error {
	slog.Info("stopping", slog.String("op", "shutdown"))
	p.ready.Store(false) // 1. /readyz → 503

	httpCtx, cancelHTTP := context.WithTimeout(context.WithoutCancel(ctx), httpGrace)
	defer cancelHTTP()
	clean := true
	if err := p.public.Shutdown(httpCtx); err != nil { // 2. приёма нет, текущие запросы дописываются
		slog.Error("requests outlived shutdown budget", slog.Any("error", err))
		_ = p.public.Close() // рвёт соединения: контексты запросов отменяются
		clean = false
	}
	if !within(p.jobs.Stop, jobsGrace) { // 3. новых прогонов нет, текущий дописывает исход
		slog.Error("jobs outlived shutdown budget", slog.String("op", "shutdown"))
		clean = false
	}
	if clean {
		p.pool.Close() // ждёт все соединения — поэтому только после чистой остановки
	}

	flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
	defer cancelFlush()
	return errors.Join(p.internal.Shutdown(flushCtx), p.obs.Shutdown(flushCtx), p.flush(flushCtx)) // 4.
}

// within ждёт stop не дольше budget; false — прогон ещё идёт.
func within(stop func(), budget time.Duration) bool {
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}
```

**Что стоит отмена посреди прогона.** Блоки дописывают сами всё, что можно
дописать:

- **`mail.Service.Deliver`** пишет известный исход мимо отмены прогона, со
  сроком `SendTimeout`, а невзятый остаток пачки возвращает в очередь
  (`FinishReleased`: попытка возвращается, место в очереди сохраняется). Под
  арендой до `Lease` остаются только письмо, отправку которого оборвала сама
  отмена, — оно могло уйти, — и строки, взятые с `Reclaimed`. Их судьбу решает
  `mail.Config.Uncertain`: `UncertainRetry` отправит повторно, `UncertainPark`
  уведёт в `failed(uncertain)` на разбор. При повисшей базе запись исхода и
  возврат остатка держат остановку до двух `SendTimeout`.
- **`outbox.Worker.Drain`** пишет ответ хендлера — `nil`, `ErrSkip`, ошибку с
  классом `Permanent` или `Throttled` — мимо отмены прогона, со сроком
  `HandlerTimeout`, а невзятый остаток пачки возвращает в очередь без
  потраченной попытки. Под арендой до `Lease` остаются только строка, чей
  хендлер оборвала сама отмена (ошибка без класса или паника при отменённом
  контексте), и строки, взятые с `Reclaimed`; после `Lease` они приходят с
  `Reclaimed`. Прогон отдаёт причину отмены, а не `ErrUnavailable`. При повисшей
  базе запись исхода и возврат остатка держат остановку до двух
  `HandlerTimeout`.
- **`objectstore.Collector.Run`** отдаёт причину отмены, а не `ErrUnavailable`:
  остановка сборщика не выглядит сбоем хранилища.

Что проект делает при остановке:

1. контекст задач сигналом не отменяется (выше): отмена обрывает текущую
   отправку письма, а её исход неизвестен;
2. бюджет `Stop` покрывает обычную пачку `mail` и ещё два `SendTimeout` на
   запись исхода и возврат остатка, так же — пачку `outbox` и два
   `HandlerTimeout`; `BatchSize` и оба срока подобраны под бюджет;
3. `Uncertain` выбирается с учётом остановки: процесс, убитый после
   исчерпанного бюджета, оставляет под арендой всю взятую пачку, и при
   `UncertainPark` каждое письмо разбирает человек;
4. единичный неуспешный прогон задачи на остановке — не инцидент: алерт по
   задаче строится на давность последнего успеха, как «крон умер» в
   [schedulerotel/doc.go](../scheduler/schedulerotel/doc.go).

**Если прогон всё же не дописан** — бюджет исчерпан или процесс убит:

| Задача | Что остаётся | Кто доделывает |
|---|---|---|
| `mail.Service.Deliver` | взятая пачка в `sending` до конца `Lease`: возврат остатка не успел | следующий `Claim`, дальше — `Config.Uncertain` |
| `outbox.Worker.Drain` | взятая пачка в `processing` до конца `Lease`: возврат остатка не успел | после `Lease` хендлер повторяется с `Reclaimed`, поэтому он обязан быть идемпотентным |
| `payment.Reconciler.Run` | курсор в памяти теряется | новый процесс идёт с головы очереди; повтор гасят ключ провайдера, дедуп событий и CAS |
| `Purge` у `mail` и `outbox`, `session.Service.Sweep`, `entitlement.Service.Run`, `objectstore.Collector.Run` | недоделанная уборка | следующий прогон |

## 6. Миграции

**Миграции блока применяет раннер проекта, блок их не запускает.** DDL-права у
приложения и гонка реплик при выкате — цена, которую выбирает проект, а не
библиотека ([ADR-0005](adr/0005-packaging.md), [ADR-0011](adr/0011-migrations-in-blocks.md)).

**Сегодня — копия `schema.sql`.** Каждый из семи `<pkg>pg` отдаёт `schema.sql` с
маркерами goose; проект кладёт файл в свои миграции как есть и сверяет
`CheckSchema` на старте. Правка копии — вторая правда о схеме: её найдёт
`CheckSchema`, но у проекта и после выката. Строку `<pkg>pg.Schema` в новом
коде не берут: волна миграций её убирает. Образец —
[`examples/monolith/migrations`](../examples/monolith/migrations).

**После волны [ADR-0011](adr/0011-migrations-in-blocks.md)** (принят, едет
минорами блоков после тегов):

- файлы едут внутри блока: `<pkg>pg.Migrations() fs.FS`, первая —
  `00001_<pkg>_init.sql`;
- у каждого блока своя таблица версий по имени модуля — `mail_schema_version`,
  хотя данные лежат в `email_outbox`: блоки выпускаются независимо, и номера в
  общей таблице столкнутся. Раннер goose собирается `goose.NewProvider` с
  `goose.WithTableName`; глобальный API goose семи таблиц не держит;
- база, где схема уже накатана копией, переходит двумя шагами: `CheckSchema`
  зелёный — затем первая миграция отмечается применённой без выполнения
  (рецепт — ADR-0011, решение 4). Таблицу версий руками не создают: goose
  кладёт в неё нулевую строку сам и без неё работать отказывается;
- выпущенная миграция не правится: изменение схемы — новый файл (решение 6).

**Раннер — отдельной командой выката до старта процессов либо шагом старта под
блокировкой раннера** (`goose.WithSessionLocker`). Реплики, стартующие разом,
иначе накатывают одно и то же параллельно; отдельная команда вдобавок оставляет
приложению роль без DDL.

**`Down` в проде не запускают.** Откат выпущенной схемы — новая миграция вперёд;
`Down` на живой базе — потеря данных под видом операции (ADR-0011, «Чего НЕТ»).

## 7. Трассировка и корреляция

**`request_id` ставит `reqid.Middleware` — первым в цепочке публичного
обработчика.** Он принимает `X-Request-Id` клиента в безопасной форме
(`[A-Za-z0-9._-]`, до 128 байт) или выдаёт свой, кладёт в контекст и отдаёт эхом
в заголовке ответа ([kit/reqid/doc.go](../kit/reqid/doc.go)). Первым — потому что
идентификатор нужен всему, что ниже: ответчику ошибок, журналу аудита, логу.
Образец — `routes` в [`examples/monolith/app.go`](../examples/monolith/app.go).

**`trace_id` есть только там, где в контексте есть спан.** `otelboot` поднимает
провайдер трейсов (noop без `OTEL_TRACES_ENDPOINT`) и при `SetGlobals` с
заданным адресом ставит W3C-пропагаторы, но спанов сам не создаёт и в лог
ничего не пишет. Спан запроса
создаёт обвязка проекта, например `otelhttp.NewHandler` с провайдером из
`otelboot`; спаны обработки событий — декоратор `outboxotel`. Нет спана — нет
ключа, и это не ошибка: трейсинг выключен.

```go
handler := reqid.Middleware(otelhttp.NewHandler(mux, "http", otelhttp.WithTracerProvider(obs.Tracer)))
```

**`request_id`, `trace_id` и `subject_id` дописывает в запись обработчик `slog`,
а не вызывающий код.** Запись без `request_id` не находится по нему никогда, а
руками его забывают. Обработчик стоит снаружи `errtrack`, и событие трекера
получает те же ключи, что строка лога (`newLogger`, раздел 1).

```go
// ctxHandler дописывает в запись идентификаторы из контекста.
type ctxHandler struct{ next slog.Handler }

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	// httperr кладёт request_id сам: второй такой ключ в JSON — дубль.
	if id := reqid.From(ctx); id != "" && !hasKey(r, "request_id") {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	if p, ok := authhttp.PrincipalFrom(ctx); ok {
		r.AddAttrs(slog.String("subject_id", p.SubjectID.String()))
	}
	return h.next.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return ctxHandler{next: h.next.WithAttrs(as)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{next: h.next.WithGroup(name)}
}

func hasKey(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}
```

**В ответ об ошибке уходят `slug` и `request_id`, `trace_id` — нет.** Тело
ответа `httperr` плоское, `{"slug", "request_id"}`, и тот же `request_id`
приходит заголовком `X-Request-Id`; причина, текст и стек уходят в лог
([httperr/doc.go](../kit/errs/httperr/doc.go), п. 1). Путь разбора: `request_id`
из ответа → запись лога → `trace_id` из той же записи → трасса. Поэтому
ответчик собирается с `RequestID: reqid.From`: без него `request_id` в теле нет,
и по ответу нечего искать. Образец — `newResponder` в
[`examples/monolith/errors.go`](../examples/monolith/errors.go).

**У фонового прогона `request_id` нет.** Его записи находятся по `job`; если
прогону нужна склейка цепочки записей, `reqid.With(ctx, id)` кладёт
идентификатор в контекст так же, как middleware.

## Короткая форма

- [ ] `slog.SetDefault` на JSON в stdout первой строкой; обработчик контекста снаружи `errtrack.WrapLogger`
- [ ] ключи — из словаря раздела 1; логина, адреса, тела письма, токена, ключа объекта, `Detail` и URL со строкой запроса в логе нет
- [ ] наблюдатель планировщика — `schedulerotel` и `scheduler.LogObserver` вместе
- [ ] окружение — одним `config.Loader`, `Err()` один раз; секреты — `Secret` и `OptionalSecret`; ослабляющих умолчаний нет
- [ ] `/metrics`, `/healthz`, `/readyz` — на `INTERNAL_ADDR`; `/readyz` зовёт `CheckSchema` каждого `<pkg>pg`
- [ ] старт: логгер → конфиг → `otelboot` → пул → блоки и `CheckSchema` → `scheduler.New` → `net.Listen` → `Start` → готов
- [ ] `jobs.Start(context.WithoutCancel(ctx))`; остановка: 503 → `Shutdown` → `Stop` с бюджетом → пул → служебный порт и сброс
- [ ] `mail.Config.Uncertain` и `BatchSize` выбраны с учётом остановки; сумма бюджетов меньше срока убийства процесса
- [ ] миграции блоков применяет раннер проекта; после волны ADR-0011 — своя таблица версий на блок
- [ ] `reqid.Middleware` первым; `httperr.Config.RequestID` — `reqid.From`

## Чего в контракте нет

Решения, а не пробелы ([ADR-0010](adr/0010-block-owns-guarantees.md), «Чего
НЕТ»): HTTP-каркаса, роутера и DI-контейнера — хватает `net/http` и функции
сборки; обязательного лога доступа — ошибку пишет `httperr`, объём трафика
видно по метрикам; экспорта логов из процесса — их забирает агент со stdout;
выбора хранилища логов, метрик и трейсов — это инфраструктура проекта.
