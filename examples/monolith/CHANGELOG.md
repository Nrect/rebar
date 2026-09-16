# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/).
Версий у примера нет и не будет: он подключает соседей через `replace` и в
теги не входит (ADR-0005).

## [Unreleased]

### Changed

- **Схемы блоков — их `Migrations()` (ADR-0011).** Копии `00001_auth.sql` …
  `00007_entitlement.sql` удалены. В каталоге монолита только его таблицы:
  `00001_shop_init.sql` — `shop_*` и каталог товаров, переименованный из
  `entitlement_products` и `entitlement_product_items` в `shop_products` и
  `shop_product_items` (префикс `entitlement_` — блока). Миграция идемпотентна
  в обе стороны: `IF NOT EXISTS`, внешние ключи на таблицы блоков —
  `DROP CONSTRAINT IF EXISTS` и `ADD CONSTRAINT`, `Down` — `IF EXISTS`, второй
  откат больше не падает. `shoppg.Migrate` — `goose.NewProvider` на каталог со
  своей таблицей версий (`<модуль>_schema_version`, у монолита
  `shop_schema_version`) под `lock.NewPostgresSessionLocker`; глобальный API
  goose снят. Порядок — блоки, затем свой каталог; накат, старт и `/readyz`
  берут блоки одним списком `schemaBlocks`. База стенда пересоздаётся.
- Выдачи пишет `entitlementpg`: `entitlement.New` и хук зачисления — `WithTx` в
  транзакции книги; его `CheckSchema` в общем списке. Повторная выдача больше
  не сокращает срок, негодный предмет отвергается `ErrInvalidGrant`.
  `shoppg.Entitlements` удалён, `shoppg.NewSettler` принимает
  `*entitlementpg.Store`.
- Старт и остановка — по `docs/CONSUMER.md`, §§4–5. `App.Start` занимает порты
  до «готов», снимает первый снимок гейджей и запускает задачи на контексте,
  который сигнал не отменяет; `App.Wait` ждёт сигнала или падения сервера;
  `App.Stop` гасит по шагам со своими бюджетами: `/readyz` → 503, публичный
  `Shutdown` (10 с), `scheduler.Stop` (15 с), пул — только после чистой
  остановки, служебный порт, `otelboot` и трекер (3 с). `cmd/monolith` —
  логгер первой строкой, конфиг, `New`, `Start`, `Wait`. Почта подобрана под
  бюджет задач: `SendTimeout` 5 с (было 10), `BatchSize` 10 (было 20) — обычная
  пачка и два `SendTimeout` на запись исхода и возврат остатка укладываются в
  15 с; держит `TestStopBudgets_FitKillDeadline`.
- `/metrics`, `/healthz` и новая `/readyz` — на служебном порту `INTERNAL_ADDR`
  (по умолчанию `127.0.0.1:9090`), с публичного роутера сняты. `/readyz`
  сверяет схему блоков тем же списком, что и старт (`blockSchemas`), причину
  пишет Warn; сверка `paymentpg` переехала в этот список из сборки.
- Логи — JSON из `log/slog` в stdout (`logotel`): записи Error уходят и в
  трекер (`errtrack`, `SENTRY_DSN`), `request_id`, `trace_id` и `subject_id`
  дописывает обработчик контекста. Логгер процесса передаётся в
  `New(ctx, cfg, log, migrations)` и дальше в `httperr`; ключ ошибки — `error`.
- Наблюдатель планировщика — тройник `schedulerotel` и `scheduler.LogObserver`:
  упавший прогон виден записью с причиной, а не только счётчиком.
- **Умолчания окружения безопасны для прода — стенд запускается с
  `stand.env`.** `SESSION_COOKIE_SECURE=true`, `SMTP_TLS=mandatory`,
  `SMTP_AUTH=plain` (вход без учётки — отказ в общем списке),
  `SMTP_ALLOW_PLAINTEXT=false`, `SMTP_PORT=587`; умолчаний нет у `ENVIRONMENT`,
  `BASE_URL`, `MAIL_FROM`, `MAIL_DOMAIN` и `SMTP_HOST`. Новые переменные:
  `ENVIRONMENT` (окружение `example` из кода снято), `LOG_LEVEL`,
  `INTERNAL_ADDR`, `OTEL_TRACES_ENDPOINT`, `SENTRY_DSN`. Тот же `stand.env`
  поднимает тесты стенда, поэтому пример окружения не отстаёт от `Load`.
- `shoppg.Entitlements`: снят устаревший комментарий «адаптера у пакета нет» —
  `entitlementpg` влит. Переход на него ждёт миграции: в копии
  `00007_entitlement.sql` нет CHECK предмета, и сверка `entitlementpg` на ней
  красная; расхождения самого `shoppg.Entitlements` с портом названы там же.

- Регистрация проверяет получателя портом `session.Recipients` до записи
  личности (Д4, правка в `auth`): у монолита правило — `mail.NormalizeAddress`,
  то же, по которому собирается письмо. Негодный адрес отвечает 400
  `login-invalid`, и повтор — снова 400, а не «принято» с личностью без письма.
  Правило `mail.ErrInvalidMessage → login-invalid` снято: из `/register` она
  больше не выходит. Совпадение правила с письмом держат
  `TestRecipients_MatchLetter` и `FuzzRecipients_MatchLetter`, отсутствие логина
  в тексте отказа — `TestRegister_RejectionTextHasNoLogin`.
- Вебхук отвечает ответчиком классом, без `Translate`: провайдеру слаги
  продукта не нужны, а правило, совпавшее с ошибкой хука глубоко под
  `payment.ErrUnavailable`, превратило бы 503 в 4xx. Держат
  `TestWebhook_AnswersClassWithoutTranslate` и
  `TestResponders_WebhookSkipsProductRules`.
- `payment.ErrInvalidMoney` → 500: сумму и валюту считает сервер из каталога,
  400 соврал бы клиенту и спрятал дефект сборки в Debug-лог. Понижение класса
  до 500 законно только с доводом в `downgrades` — это держит
  `TestTranslate_NoRedundantRule`.
- Таблица перевода ошибок `errors.go` сжата с 38 строк до 14 после волны
  ADR-0007: класс несёт sentinel модуля, и `httperr` без правила отвечает
  статусом класса и слагом — именем класса. Сняты `unavailableRules` целиком;
  слаги, которые лишь переименовывали класс (`no-session`,
  `too-many-attempts`, `access-denied`, `idempotency-key-invalid`,
  `idempotency-key-reused`, `amount-invalid`, `webhook-not-authentic`,
  `webhook-malformed`, `payment-unsupported`, `file-too-large`); мёртвые — на
  sentinel, которую монолит наружу не отдаёт (`password.ErrBusy`,
  `entitlement.ErrDenied`, `auth.ErrLoginTaken`, `payment.ErrIdempotencyRace`,
  `payment.ErrReferenceBusy`, `payment.ErrUnknownIntent`); неверные
  (`purchase-invalid` на `payment.ErrInvalidRequest` — покупку собирает код,
  это 500; `file-not-found` на `objectstore.ErrNotFound` — это нет бакета).
  Добавлено `mail.ErrInvalidMessage → 400 login-invalid`: адрес письма здесь —
  логин из формы, и регистрация с негодным адресом отвечала 503. Для клиента:
  503 теперь `unavailable`, 501 — `not-implemented`, 409 на чужой ключ —
  `conflict`. `allowed` отвечает `session.ErrNoSession`, а не своим
  `no-session`.
- Первый снимок гейджей — сразу при старте: `App.Start` зовёт `RunNow` задачи
  `gauges_snapshot` до запуска планировщика. Без него первую минуту после
  деплоя `payment_drift` — денежный алерт с порогом 1 — был бы слеп. Держит
  `TestStart_SnapshotsGaugesBeforeFirstTick`.
- Снимки гейджей обновляет отдельная задача `gauges_snapshot` на своём такте
  `GAUGES_TICK` (по умолчанию минута); `/metrics` снова голый обработчик
  `otelboot`, и scrape в базу не ходит (CONVENTIONS §6).
- Проведены `paymentotel.Wrap` (провайдер оборачивается до `NewService`,
  `payment_provider_calls_total{provider,type,result}`) и `paymentotel.NewGauges`
  (`payment_intents_stuck`, `payment_drift`). Тесты: декоратор согласован с
  ответом сервиса на четырёх отказах (`rejected`=3, `error`=1), гейдж зависших
  кормится задачей, а scrape его не обновляет.
- Пересадка на правки `payment` (`claude/payment-fixes-abc`, `70f6053`):
  обязательный `payment.Observer` проведён через `paymentotel.NewObserver` на
  общем метре — `payments_total{op,reason}` в `/metrics` с нулём на каждой
  паре; `payment.ErrUnsupported → 501 payment-unsupported` — без правила
  окончательный отказ упал бы в 500, когда `payment` перестал заворачивать его
  в `ErrUnavailable`. `TestErrorClasses_ReachHTTP` проверяет отказ провайдера
  по обоим путям (статусом и ошибкой), «не умеет» и временный сбой. Двойник
  провайдера — на сеттерах под замком.
- Пересадка на исправленные порты: шесть обходов сняты, потому что порты
  сошлись. `shoppg.Entitlements` переписан под `Store.Grant(…, at)` —
  `GrantAt`, часы адаптера и `SetClock` ушли целиком; `Open` читает
  `granted_at` обратно. Копия `sameLetter` заменена на `mail.CheckDuplicate`,
  чтение мимо домена — на `payment.Service.IntentByKey`, свой
  `ErrAccessDenied` — на `authz.ErrDenied`, ручная сборка схемы прогона — на
  `pgtest.SchemaDSN`, пароль SMTP — на `config.Loader.OptionalSecret`.
- Сквозной тест проверяет, что `granted_at` равен моменту записи книги, а не
  времени адаптера: без этого адаптер с `DEFAULT now()` прошёл бы сценарий.

### Added

- Тесты миграций на живой базе: `TestMigrate_EmptyBase` — накат на пустую
  базу, восемь таблиц версий без общей `goose_db_version`, сверка каждого
  блока зелёная, включая `entitlementpg`; `TestMigrate_ReapplyAppliesNothing` —
  повторный накат не применяет ничего ни у одного провайдера, а `Up` монолита,
  выполненный заново мимо раннера, оставляет данные и внешние ключи;
  `TestMigrate_DownTwiceThenUp` — откат всех каталогов в обратном порядке
  раннером, затем секциями `Down` ещё раз, затем накат снова зелёный.
  `TestMigrations_Catalog` — стражи номеров и секций каталога.
- `TestStop_RunningJobWritesOutcome`: сверка платежей по расписанию задержана
  у провайдера, приходит сигнал — остановка ждёт прогон, и исход ложится в базу.
  `TestReadyz_SchemaMismatchIs503`: несошедшаяся колонка блока — 503 и Warn, а
  служебные ручки на публичном порту не отвечают. `TestStart_BusyPortRefuses`,
  `TestJobs_FailureIsLogged`, `TestLogs_HTTPErrorCarriesIDs`, `TestLoad_*` —
  безопасные умолчания и список обязательных; `TestStopBudgets_FitKillDeadline`
  — сумма бюджетов остановки и бюджет задач под пачку `mail`; тесты `logotel`.
- Стражи таблицы ошибок: `TestTranslate_SentinelsReachHTTP` — итоговые статус
  и слаг каждой sentinel, которую монолит отдаёт наружу, через настоящий
  ответчик; `TestTranslate_NoRedundantRule` — правило со статусом класса без
  довода в `clientActs` лишнее; `TestTranslate_EveryRuleReachable` — правило на
  sentinel, которой нет ни на одном пути наружу, мёртвое;
  `TestRegister_BadAddressIsInput`.
- `TestSettlerSlugError_KeepsWebhookRetryable`: `errs.Conflict` из хука
  зачисления под `payment.ErrUnavailable` отвечает 503, а не 409, и повтор
  провайдера применяет оплату; на `kit v0.2.0` тест красный с 409.
- `payments_reconcile` под `postgres/pglock`: сверка не захватывает строки
  (курсор в памяти), и две реплики шли бы одной очередью, умножая вызовы
  провайдера; `payment.Reconciler` сам требует внешнюю блокировку. Остальным
  пяти задачам она лишняя или вредна — разбор в `doc.go`. Наблюдатель
  `lockotel` — `cron_lock_total{job,result}` с нулями на старте.
  `TestPaymentsReconcile_OneReplicaRuns`: две реплики на одной базе, провайдер
  позван сверкой один раз, вторая видна как `skipped`, хотя планировщик
  записал её прогон успехом.
- `examples/monolith` — потребитель на всех двенадцати модулях тулкита: ручки
  на `net/http` без роутера, пять фоновых задач в `scheduler`, миграции всех
  адаптеров в `migrations/` и сквозной тест на поднятом стенде.
- `shoppg` — адаптеры потребителя: `auth.Identities`, `session.Tokens`
  целиком, `paymentpg.Settler`, `entitlement.Store` по эталонной схеме,
  заказы и загруженные файлы.
- Сквозной сценарий `TestMonolith_EndToEnd`: регистрация → письмо → ссылка →
  вход → checkout → вебхук → outbox → письмо об оплате → загрузка → метрики.
- `TestSettlerFailure_RollsBackEverything` — падение хука откатывает всё,
  включая строку дедупа события.
- `TestErrorClasses_ReachHTTP`, `TestErrorBody_LeaksNothing` — 503 и 403
  различимы, в теле ответа нет ни строки базы, ни логина, ни токена.
- `TestStart_RefusesBadTransportConfig` — опечатка в настройках почты роняет
  сборку приложения, а не первое письмо.
- `SMTP_TRANSPORT=smtp|unconfigured` — закрытый набор с `AllTransportModes`:
  стенд без почтовика выбирается ЯВНО, а не достаётся исходом ошибки.
  Нулевое значение — отказ; guard-тесты держат набор, умолчание и оба рубежа
  отказа (конфиг и сборка).
- Стражи: драйвер и otel не выходят за пределы своих каталогов.

### Fixed

- SIGTERM отменял контекст фоновых задач: прогон обрывался посреди работы — у
  `mail` письмо посреди отправки оставалось в `sending`, а после `Lease` его
  цену выбирал `Uncertain`. HTTP гасился одновременно с отменой задач, а `Stop`
  шёл `defer`'ом без бюджета.
- Занятый порт всплывал ошибкой из горутины `ListenAndServe` уже после старта.
- Схема сверялась в двух местах (`checkSchemas` и `startMoney`).
- Повтор ключа в `checkout` разбирает `payment.Start`, а не проба
  `IntentByKey` до него. Проба отдавала любое найденное намерение успехом:
  другой товар под тем же ключом получал 200 с чужой ценой и ссылкой, повтор
  после отказа провайдера — 200 без ссылки вместо 409, протухшая попытка —
  200 вместо `payment-closed`. id заказа выводится из плательщика и ключа
  (`ON CONFLICT DO NOTHING`), поэтому повтор приходит в `Start` тем же
  запросом и второй строки заказа не заводит. Держат `TestCheckout_*`.
- Ключ идемпотентности проверяется `payment.NormalizeKey` до заказа: негодный
  больше не оставляет заказ без намерения (`TestCheckout_BadKeyLeavesNoOrder`).
- Ключ дедупа писем — вид плюс SHA-256 токена (у писем без ссылки — адреса),
  как советует `mail`. Ключ из логина и срока переполнял `mail.MaxKeyLen` у
  логина длиннее 173 байт: регистрация отвечала 503, личность оставалась без
  письма, а повтор — 202 (`TestRegister_LongAddressGetsLetter`).
- Гейджи `mail` и `outbox` обновлялись на каждом scrape: частоту запросов к
  базе задавал Prometheus, умноженный на реплики и скрейперы, — вопреки
  CONVENTIONS §6. Теперь их обновляет `gauges_snapshot`.
- Ошибки `smtp.New` и `mailotel.Wrap` больше не проглатываются: обе фатальны
  на старте. `smtp.New` к сети не ходит, поэтому терпелась только опечатка в
  конфиге — приложение стартовало, а письма подтверждения копились в очереди
  с `ErrTransportUnconfigured`. `mail.Unconfigured` — сознательное отсутствие
  транспорта, а не запасной вариант на негодный конфиг; выбирается режимом
  `SMTP_TRANSPORT`.
