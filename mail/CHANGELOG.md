# Changelog — mail

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- `mailtest.RunStoreSuite(t, factory)` — контрактный набор порта `mail.Store`
  ([CONVENTIONS §5](../CONVENTIONS.md#5-тесты)) на `testing`, без testify.
  Гоняется в одном бинаре по `mailtest.MemStore` и по `mailpg.Store`
  (`TestStoreContract`). Сценарии — моменты как из `timestamptz`: возврат в
  UTC и до микросекунд, возраст в `Stats` от усечённого момента, аренда и
  `Purge` на границе одной микросекунды. Остальной контракт порта по-прежнему
  держат интеграционные тесты `mailpg`.
- Сценарий набора «отменённый контекст — ErrUnavailable»: `Enqueue`, `Claim`,
  `Finish`, `Stats` и `Purge` на отменённом контексте отвечают
  `mail.ErrUnavailable` с `context.Canceled` в цепочке. Расхождение двойника и
  адаптера на отмене теперь краснеет в наборе, а не у потребителя.

### Changed
- **`mailtest.MemStore` замечает отменённый контекст: каждый метод порта
  отвечает `mail.ErrUnavailable` с `context.Canceled` в цепочке — как
  `mailpg`, где отмена не доезжает до базы.** Раньше двойник контекст не
  смотрел, и тест потребителя «отменили запрос — записи не случилось» был
  зелёным на двойнике и красным в проде. Непозитивный лимит `Claim` остаётся
  пустой выборкой без ошибки и на отменённом контексте: `mailpg` на нём в базу
  не ходит. Ломающее для теста потребителя, который передавал в двойник
  отменённый контекст и ждал успеха.
- **`mailtest.MemStore` хранит и отдаёт моменты как `timestamptz`: в UTC и с
  точностью до микросекунд, — и с той же точностью сравнивает параметры
  (аренда в `Claim`, `Purge`).** Раньше двойник отдавал время как передали, с
  наносекундами и зоной: сравнение меток у потребителя было зелёным на
  двойнике и красным на базе, а наносекунда после конца аренды отдавала
  второму воркеру строку, которую база ещё держит. Контракт записан у
  `mail.Store`. Ломающее для теста потребителя, который сравнивал момент
  строки двойника голым `==` или `assert.Equal` с часами в чужой зоне или с
  наносекундами, — на базе такой тест был красным всегда.
- `doc.go`: на пути вставки в транзакцию класс ошибки принадлежит потребителю
  — вставка идёт его хендлом в его транзакции, ядро ошибку не видит и не
  заворачивает; `mailpg` заворачивает сбой в `ErrUnavailable` сам.
- **Класс ошибки у sentinel ([ADR-0007](../docs/adr/0007-error-kind.md)).**
  Потребителю больше не нужна таблица перевода ошибок `mail`: `errs.KindOf` и
  `httperr` находят класс на самой sentinel. Страж
  `errstest.EveryErrorHasKind` стоит в корне модуля (`sentinels_test.go`);
  двойники `mailtest` из него исключены — своего класса у их sentinel нет:
  сбой хранилища двойник заворачивает в `ErrUnavailable` сам, как `mailpg`
  (ниже), а причину стоп-листа отдаёт голой, и класс ей даёт обёртка ядра —
  это держит `TestPortFailuresReachCallerAsUnavailable` на `Suppress`.

  | Sentinel | Класс | Почему |
  |---|---|---|
  | `ErrUnavailable` | 503 `unavailable` | сбой хранилища или стоп-листа, повтор осмыслен |
  | `ErrTransportUnconfigured` | 503 `unavailable` | временный сбой (ADR-0001, «Транспорты») |
  | `ErrInvalidMessage` | нет, `//errs:nokind` | адрес и заголовки собирает код потребителя: модуль не знает, пришёл адрес из формы или из шаблона; потребитель с открытой формой адреса ставит класс одним правилом `Translate` (ADR-0007, «Спорные назначения») |
  | `ErrKeyReused` | нет, `//errs:nokind` | ключ дедупа строит код потребителя (`"verify:<id>"`), поэтому тот же ключ на другое письмо — ошибка ключа в коде, а не конфликт для клиента; у `payment.ErrIdempotencyKeyReused` ключ присылает клиент, отсюда её 409 |
  | `ErrBadKind`, `ErrKeyInvalid`, `ErrNoSuppressor`, `smtp.ErrInvalidConfig` | нет, `//errs:nokind` | тип, ключ и сборку задаёт код потребителя: негодные — дефект, то есть 500 |

- **Ломающее для кода, который сравнивал тексты sentinel, присваивал их,
  ссылался на `ErrSuppressed`, различал `mail.Backoff` и `outbox.Backoff` как
  типы или звал `SetClock(nil)`.** Замена:

  | Было | Стало |
  |---|---|
  | тип sentinel с классом — `error` | `errs.KindError`; `errors.Is` и `==` работают как прежде |
  | `message is invalid`, `unknown message kind`, `dedup key …` | тот же текст с префиксом `mail: ` |
  | `mail operation could not be completed` | `mail: operation could not be completed` |
  | `mail suppressor is not configured` | `mail: suppressor is not configured` |
  | `mail transport is not configured` | `mail: transport is not configured` |
  | паника `NewService`: `… must be a valid address: message is invalid: …` | `… must be a valid address: mail: message is invalid: …` |
  | `ErrSuppressed` (`recipient is suppressed`) | удалена, заменителя нет: исход стоп-листа — статус строки `suppressed` (`FinishSuppressed`), а не ошибка. Ни один путь пакета её не возвращал, и `errors.Is(err, mail.ErrSuppressed)` у потребителя был бы вечно ложным |
  | `mail.Backoff` — свой тип пакета | `mail.Backoff = retry.Backoff` (псевдоним): литерал `mail.Backoff{Base, Max}` компилируется как прежде, у типа появился `Delay`; ломается код, различавший `mail.Backoff` и `outbox.Backoff` как разные типы (оба в одном type switch, `%T`, reflect) |
  | `Service.SetClock(nil)` принимался и падал разыменованием при первом обращении к часам | паника `mail.Service.SetClock: now must not be nil` |

  Префикс не косметика: `KindError` равны по классу и тексту, и без него
  `mail.ErrUnavailable` совпала бы через `errors.Is` с `outbox.ErrUnavailable`.
  Модуль требует `github.com/nrect/rebar/kit v0.2.0`.
- `Backoff` берётся из `kit/retry`: копия экспоненты с джиттером удалена.
  Поведение то же — сверено построчно: тело и структура копии совпадали с
  `kit/retry`. Расходился только комментарий: он называл формулу
  `Base·2^attempt`, а считала копия, как и `kit`, `Base·2^(attempt−1)`. Тесты
  границ backoff остались в модуле и гоняют `kit` через псевдоним.
- **API двойников меняется ломающе: настройка `mailtest.Transport`,
  `mailtest.SESServer`, `mailtest.MemStore` и `mailtest.MemSuppressor` —
  методы, а не публичные поля.** Двойники читали поля под своим мьютексом, а
  тест писал их мимо него. Пока поле ставится до первого вызова, гонки нет; но
  тест потребителя через живой HTTP-сервер пишет поле, пока ручка сервера его
  читает, — и `-race` краснеет у потребителя. `FailFor` и `ThrottleFor`
  двойники к тому же сами декрементируют под замком, и запись мимо замка
  роняет процесс `fatal error: concurrent map writes` — упавший прогон, а не
  красный тест. Теперь настройка правится под тем же замком, хуки зовутся вне
  его и вправе звать сам двойник
  ([CONVENTIONS §3](../CONVENTIONS.md#3-двойники)). Замена:

  | Было | Стало |
  |---|---|
  | `tr.RejectFor[email] = code` | `tr.RejectFor(email, code)` |
  | `tr.FailFor[email] = n` | `tr.FailFor(email, n)` |
  | `tr.SendHook = hook` | `tr.SetSendHook(hook)` |
  | `srv.RejectFor[email] = code` | `srv.RejectFor(email, code)` |
  | `srv.ThrottleFor[email] = n` | `srv.ThrottleFor(email, n)` |
  | `srv.Secret = secret` | `srv.SetSecret(secret)` |
  | `srv.Region = region` | `srv.SetRegion(region)` |
  | `srv.StoreLimit = n` | `srv.SetStoreLimit(n)` |
  | `srv.Name = name` | `srv.SetName(name)` |
  | `srv.OnAccepted = hook` | `srv.SetOnAccepted(hook)` |
  | `store.Err = err` | `store.SetErr(err)` |
  | `store.FinishErr = err` | `store.SetFinishErr(err)` |
  | `supp.Err = err` | `supp.SetErr(err)` |

  Счётчики снимаются нулём, ошибки и хуки — `nil`. Замены `delete` по карте
  `RejectFor` нет: у транспорта поведение, меняющееся по ходу теста, задаётся
  `SetSendHook`. Флаги и окружение `cmd/sesfake` не менялись.
- **Сбой хранилища `mailtest.MemStore` приходит в `mail.ErrUnavailable`, как у
  `mailpg` ([ADR-0007, «Двойники»](../docs/adr/0007-error-kind.md)).** Было:
  `SetErr` и `SetFinishErr` отдавали причину голой из `Enqueue`, `Claim`,
  `Finish`, `Stats` и `Purge`, `ErrIDReused` тоже была голой, и потребитель,
  зовущий стор мимо сервиса (вставка в своей транзакции), видел на двойнике
  класс 500 там, где прод отвечает 503. Стало: `errs.KindOf` — `unavailable`,
  а `errors.Is` находит и `mail.ErrUnavailable`, и причину. Ломающее для
  теста, который ветвился по голой ошибке двойника: `errors.Is` по причине
  истинен по-прежнему, а `NotErrorIs(err, mail.ErrUnavailable)` и сравнение
  текста — уже нет. `MemSuppressor` и `Transport` отдают причину голой, как
  раньше: стоп-лист пишет потребитель, а `smtp` и `sesv2` временный сбой не
  классифицируют.

### Fixed
- **Отмена `Deliver` посреди отправки больше не стоит дубля или ложного
  `failed`: исход письма пишется мимо отмены прогона.** Было: письмо уже ушло,
  а `Finish` шёл по тому же отменённому контексту и до базы не доезжал —
  строка оставалась в `sending`, после `Lease` её забирал следующий прогон с
  `Reclaimed`, и при `UncertainRetry` письмо уезжало второй раз, а при
  `UncertainPark` строка уходила в `failed` с `FailUncertain`, хотя письмо
  доставлено. Стало: `Finish` идёт по `context.WithoutCancel` со сроком
  `Config.SendTimeout` — новое поле не заведено: это потолок шага строки,
  который потребитель уже задал, и повисшая база держит остановку процесса не
  дольше него. Срок действует и без отмены: запись исхода дольше
  `SendTimeout` — сбой `Finish`, `ErrUnavailable`. Держат
  `TestDeliver_CancelDuringSendRecordsOutcome` — в ядре и в `mailpg` на живой
  базе — и `TestDeliver_HungFinishDoesNotOutliveBudget`.

  Во что отмена обходится теперь:

  | Отмена пришла | Строка |
  |---|---|
  | между письмами | не тронута: её ловит `waitTurn` до отправки. Ждёт `Lease`, дальше решает `Config.Uncertain` — при `UncertainPark` уйдёт в `failed`, не отправлявшись |
  | посреди отправки, а провайдер ответил | исход записан |
  | посреди отправки и оборвала её | исход НЕ записан: письмо могло уйти. Ждёт `Lease` и `Config.Uncertain`, как при падении процесса — записанный повтор обошёл бы `UncertainPark`, а исчерпанная попытка сожгла бы письмо |
  | посреди проверки стоп-листа | записан повтор: письмо не уходило |

  `Deliver` с `context.Canceled` без `ErrUnavailable` в цепочке — остановленный
  прогон, а не сбой хранилища.
- `mailpg.Purge` сортировал удаляемые строки только по `updated_at`: при равных
  моментах и лимите меньше группы база удаляла произвольные из равных, а
  `mailtest.MemStore` — первые по `id`. Теперь `ORDER BY updated_at, id`, как в
  `mailpg` `Claim` и у `outboxpg`; схема и индекс не меняются. Держит сценарий
  «Purge при равных updated_at удаляет первые по id» в `mailtest.RunStoreSuite`,
  он гоняется и по двойнику, и по `mailpg.Store`. Порядок записан в контракте
  `mail.Store.Purge`.
- `mailtest.MemStore.Stats` отдавал отрицательный `OldestPendingAge`, когда часы
  потребителя позади `created_at`, а `mailpg` в том же случае отдаёт ноль —
  двойник был мягче базы по знаку. Теперь ноль и у двойника; держит сценарий
  «часы позади строк» в `mailtest.RunStoreSuite`, он гоняется и по
  `mailpg.Store`. Контракт записан у `mail.Store.Stats`.
- `mailtest.SESServer.Sent()` отдавал письма мелкой копией: `Headers` и
  `ReplyTo` делили память с хранилищем двойника, а хук `OnAccepted` получал то
  самое письмо, что легло внутрь, — правка полученного доезжала до
  «провайдера». Теперь копия до последнего указателя в обоих местах
  ([PATTERNS §7](../docs/PATTERNS.md#7-двойник-вместо-мока), п. 7).

## [0.2.0] — 2026-09-10

Минорный, а не патч: у модуля появилась зависимость на
`github.com/nrect/rebar/postgres` — граница ошибки теперь общая, своей копии не
осталось. Плюс схема получила имена трём безымянным CHECK; тому, кто копировал
её из `v0.1.0`, нужна миграция — текст ниже, в разделе `Changed`.

### Added
- `CheckDuplicate(env, res)` — сверка отпечатка для ТРАНЗАКЦИОННОГО пути
  вставки (`Service.Prepare` + `mailpg.WithTx().Enqueue`), зеркало
  `outbox.CheckDuplicate`. До неё сверка была заперта внутри `Service.Enqueue`,
  а `sameMessage` не экспортирован — то есть `ErrKeyReused` на транзакционном
  пути был недостижим, и громкая идемпотентность (`doc.go`, п. 4) молча
  становилась тихой у каждого, кто кладёт письмо своей транзакцией. Текст
  ошибки прежней дисциплины: ни ключа, ни темы, ни тела письма. `Enqueue`
  теперь зовёт её же — сверка одна на оба пути.

### Fixed
- `TestStore_Enqueue_ErrorHidesBody` проверял только текст ошибки и потому был
  стражем, который молчит всегда: `Detail` у `pgconn.PgError` в `Error()` не
  печатается, он достаётся через `errors.As`, — то есть тест оставался зелёным
  и на адаптере, заворачивающем ошибку драйвера целиком. Теперь тест сперва
  доказывает, что в `Detail` лежит тело письма, и затем требует
  `NotErrorAs` до `*pgconn.PgError`. Граница `storeError` была верной; ошибка
  была в том, что её никто не сторожил.

### Changed
- **Все безымянные CHECK получили имена: `email_outbox_status_chk`,
  `email_outbox_fail_reason_chk`, `email_outbox_attempts_chk`.** Соседние
  `_body_cleared_chk` и `_lock_chk` имена имели с первого дня — безымянными
  остальные три были недосмотром, а не решением.

  Это **одна миграция** для того, кто копировал схему `v0.1.0`: там эти три
  ограничения объявлены без имени, и Postgres выдал им свои. После обновления
  `CheckSchema` скажет на старте, каких ограничений нет — это не поломка, а то,
  ради чего он и зовётся. Имена проверены на схеме тега `mail/v0.1.0`:

  ```sql
  ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_status_check;
  ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_status_chk
      CHECK (status IN ('pending','sending','sent','failed','expired','suppressed'));

  ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_fail_reason_check;
  ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_fail_reason_chk
      CHECK (fail_reason IN ('', 'rejected','exhausted','uncertain'));

  ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_attempts_check;
  ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_attempts_chk
      CHECK (attempts >= 0);
  ```

  Проверки не меняются — меняются только имена, поэтому пересчёта строк и
  простоя миграция не требует. Момент выбран самый дешёвый: у схемы ещё нет
  потребителей. Через месяц это была бы миграция у живого магазина.
- Имена стали контрактом, а не строкой в файле: `TestSchemaFile_HoldsContract`
  требует все пять в схеме, `expectedChecks` — в базе потребителя, табличный
  тест `CheckSchema` проверяет, что пропажа названа по имени, а
  `TestStore_Enqueue_ErrorHidesBody` требует литералом имя ограничения, которым
  база отбивает вставку (`email_outbox_body_cleared_chk`: строка с телом и
  чужим статусом нарушает сразу два CHECK, и Postgres называет это).
  Одного места хватило бы, чтобы имя разъехалось при следующей правке.

- `mailpg` переведён на общую границу ошибки `postgres.Sanitize` — своя копия
  была последней из пяти адаптеров. Копия вела себя верно, но теряла имя
  ограничения: у четырёх соседей ошибка разворачивается в `*postgres.Error` с
  `Constraint`, и потребитель отличает один конфликт от другого, а у `mail` не
  мог. Граница ошибки — граница безопасности (в `PgError.Detail` лежит тело
  письма со ссылкой и токеном), и пять копий — пять шансов разойтись ровно
  там, где расхождение стоит утечки.
- `github.com/nrect/rebar/postgres` — прямая зависимость модуля (ADR-0005,
  вторая межмодульная зависимость: только каталогам адаптеров хранилища).
  Страж импортов разрешает её **только** каталогу `mailpg`; в ядре `mail` тот
  же импорт роняет `TestPackageImportsAreWhitelisted`. Для потребителя это
  новая запись в графе модулей — то есть минорный бамп; номер версии
  назначается при тегировании.
- Интеграционные тесты `mailpg` переехали на общий стенд `postgres/pgtest`:
  свой `TestMain` всегда звал `GenericContainer`, поэтому мутационный прогон
  адаптера был невозможен — каждый мутант поднимал свой Postgres. `pgtest.Start`
  умеет `TEST_DATABASE_URL` и подметает базы, брошенные прерванным прогоном.
  Своя копия разбора goose-секций снята вместе с её регрессным тестом:
  маркеры — `pgtest.GooseUpMarker` и `pgtest.GooseDownMarker`, правда о строке
  директивы одна. `testcontainers-go` остаётся прямой зависимостью модуля из-за
  Mailpit в `mail/smtp` — к Postgres она больше отношения не имеет.
- `TestStore_Enqueue_ErrorHidesBody` дополнен проверкой типа границы:
  `ErrorAs` до `*postgres.Error` с тем же именем ограничения, которое отдаёт
  драйвер, — рядом с уже стоявшим `NotErrorAs` до `*pgconn.PgError`.
- Минимальная версия Go — 1.26.0 (была 1.25.0): её требует `golang.org/x/crypto`
  v0.56.0, обновлённый ради GO-2026-6253/6354/6355. Единая версия во всех
  модулях тулкита — правило VERSIONING, «Единая версия Go и общих
  зависимостей»; потребителю на Go 1.25 нужен бамп компилятора.

### Added
- `TestSchemaFile_ChecksMirrorClosedSets` — guard «CHECK ⊇ `All*`» для обоих
  закрытых наборов, доезжающих до базы (`AllStatuses` и `AllFailReasons`).
  У `mailpg` его не было вовсе, хотя комментарий у `mail.AllStatuses` обещал
  его («держит guard-тест и CHECK адаптера») и требует
  [CONVENTIONS §9](../CONVENTIONS.md#9-схема-адаптера). Раньше он и не мог
  существовать: адресовать безымянный CHECK нечем. Расхождение словаря кода и
  словаря базы иначе всплывает только в проде — на первом новом значении база
  отобьёт строку, которую домен считает законной.

  У причин отказа сверка ИМЕННО «⊇», а не равенство: в колонке живёт ещё пустая
  строка — «не падало», и в `AllFailReasons` её нет и быть не должно. Лишнее
  сверяется поимённо, а не длиной, поэтому чужое значение в CHECK тест поймает,
  а соблазна дописать `''` в закрытый набор ради зелёного теста не возникнет —
  пустая причина стала бы законным исходом `Finish`.

## [0.1.0] — 2026-09-07

### Security
- Toolchain go1.26.6: закрывает GO-2026-5026, GO-2026-5972, GO-2026-6089,
  GO-2026-6090, GO-2026-6218 в stdlib (net/http, crypto/tls, net/url,
  encoding/asn1), до которых дотягиваются sesv2 и httptest; govulncheck на
  1.26.5 был красным.

### Added
- Каркас пакета: типы (`Message`, `Envelope`, закрытые наборы `Status`,
  `FailReason`, `SuppressReason`), порты (`Store`, `Transport`, `Suppressor`),
  `Config` с panic-валидацией, чистый `Service.Prepare`, страж импортов.
- ADR-0001 с проектом outbox, транспортов и двойников; 2026-09-05 принят
  (вопросы 2–10 закрыты владельцем).
- `Unconfigured` — транспорт для прода без провайдера: `Deliver` с ним очередь
  не трогает, письма ждут в pending; прямой `Send` — `ErrTransportUnconfigured`
  (временный сбой). `Envelope.Reclaimed` — транзитный флаг строки, взятой из
  sending с истёкшей арендой (политика `Uncertain`).
- Ядро outbox: `Service.Enqueue` (Prepare + вставка; повтор ключа с тем же
  отпечатком — успех, с другим — `ErrKeyReused`), `Service.Deliver` (пачка с
  арендой, стоп-лист, отправка с `SendTimeout`, исходы sent/failed/expired/
  suppressed/retry, пауза `MinSendGap`, остановка пачки по отмене `ctx` и по
  сбою `Finish`), `Service.Purge`, `Service.Stats`, `Service.Suppress`.
  Транспорт `Unconfigured` очередь не трогает — попытки не тратятся.
  `LastError` усекается до `MaxErrorLen` = 500 байт по границе руны;
  новый sentinel `ErrNoSuppressor`.
- Двойники `mailtest`: `MemStore` (уникальность `DedupKey`, аренда и
  SKIP-LOCKED-семантика с `Reclaimed`, стирание тела в терминальном статусе,
  `Err`/`FinishErr`, снимки `Rows`/`Get`), `Transport` (`RejectFor`,
  `FailFor`, `SendHook`, `Sent`), `MemSuppressor`. Ошибки двойников
  (`ErrIDReused`, `ErrSendFailed`) отличимы от доменных.
- `mailotel` — наблюдаемость на OpenTelemetry metric API. `Wrap` — декоратор
  `mail.Transport` со счётчиком `emails_sent{type,result}` (unit `{email}`;
  Prometheus отрисует `emails_sent_total`): `type` — `Kind` из закрытого набора
  `Config.Kinds`, `result` — `ok` / `rejected` (`mail.IsRejected`) / `error`;
  адрес, код провайдера и текст ошибки в метки не попадают, тело не читается.
  `Name()` пробрасывается без изменений — иначе шаг 0 `Deliver` не узнал бы
  `Unconfigured`; `SendResult` и ошибка `next` возвращаются как есть.
  `NewGauges` — три observable gauge на одном коллбэке (`email_outbox_pending`,
  `email_outbox_oldest_pending_age` в секундах, `email_outbox_failed`), читающие
  снимок `mail.Stats`: снимок кладёт потребитель через `Gauges.Set` после
  прогона `Deliver`, запроса в БД на scrape нет (CONVENTIONS §6);
  `Gauges.Unregister` снимает коллбэк (идемпотентен). Nil-порт и nil-метр —
  паника в конструкторе.
- Адаптер `smtp` — `mail.Transport` на go-mail v0.8.1. TLS по умолчанию
  mandatory; `TLSNone` и пароль по открытому соединению — только с
  `AllowPlaintext`. SMTP 5xx на любой стадии и конверт без адреса →
  `*mail.RejectedError`, остальное (4xx, сеть, TLS, таймаут) — временный сбой;
  текст ошибок без адреса, темы и тела. Queue id из ответа на DATA (Postfix,
  Mailpit, Exim) → `SendResult.ProviderMessageID`; ошибка после принятого DATA
  считается успехом. `ctx` приоритетнее `Config.Timeout`. Интеграционный тест
  на Mailpit (testcontainers, пропуск по `-short`).
- `sesv2` — адаптер `Transport` для SES v2-совместимого HTTP API (Yandex Cloud
  Postbox и AWS SES): `POST /v2/email/outbound-emails` с Simple-контентом,
  SigV4 на stdlib (без AWS SDK), только https (http — loopback либо явный
  `AllowInsecureEndpoint`), классификация ответов: 4xx `MessageRejected`,
  `BadRequestException`, `MailFromDomainNotVerifiedException`,
  `AccountSuspendedException`, `NotFoundException` → `*mail.RejectedError`;
  429, 5xx, 403, `SendingPausedException`, `LimitExceededException`, сеть,
  редирект, неразобранный ответ → временный сбой (`*sesv2.ProviderError`).
  `Reply-To` из белого списка ядра уходит полем `ReplyToAddresses`;
  `Message-ID` провайдеру не передаётся — оба API запрещают его в
  пользовательских заголовках и ставят свой.
- `mailtest.SESServer` — httptest-фейк SES v2: проверка формы SigV4 (403 без
  подписи, как у SES), полная проверка подписи по `Secret`, запрет заголовков
  провайдера (`Message-ID`, `Reply-To`, `From`, …) и второго получателя,
  `RejectFor`/`ThrottleFor` по адресу, `Sent()` с разобранным письмом.
  Потокобезопасен.
- `internal/sesfake` — обработчик SES v2 без зависимости от `testing`: общее
  ядро `mailtest.SESServer` (теперь тонкая обёртка над `httptest.Server`) и
  бинаря стенда. Добавлены `StoreLimit` (хранить последние N писем) и `Reset()`.
  Имя заголовка проверяется как token RFC 5322, значение — на управляющие
  символы (CR/LF в `Content.Simple.Headers` → 400, как у провайдера).
  Публичный API `mailtest` не изменился.
- `cmd/sesfake` — SES v2-фейк для dev/stage с релеем принятых писем в Mailpit по
  SMTP (plain, без AUTH и TLS): цепочка стенда `backend → sesv2 → sesfake → SMTP
  → Mailpit`. Флаги `-listen`, `-secret`, `-region`, `-relay`, `-reject
  email=Code`, `-store-limit` дублируются переменными `SESFAKE_*`; ручки
  `POST /v2/email/outbound-emails`, `GET`/`DELETE /store`, `GET /healthz`;
  таймауты `http.Server` и graceful shutdown по SIGINT/SIGTERM с ограниченным
  ожиданием начатых релеев. Лог релея — id письма, стадия и код SMTP; текст
  ответа сервера в лог не идёт (он называет адрес получателя). Multi-stage
  `Dockerfile` (distroless/static:nonroot); образ никуда не публикуется.
- Адаптер `mailpg` — `mail.Store` на pgx/v5 и `schema.sql` для goose (таблица
  `email_outbox`, CHECK'и `email_outbox_body_cleared_chk` и
  `email_outbox_lock_chk`, индексы `ux_email_outbox_dedup`,
  `ix_email_outbox_due`, `ix_email_outbox_terminal`); файл копируется в каталог
  миграций потребителя как есть, раннера миграций в пакете нет. `New(pool)` —
  для фоновой доставки, `WithTx(tx)` — вставка письма в транзакции
  бизнес-факта (доказано `TestStore_WithTx_IsAtomic`). Повтор по `dedup_key`
  разбирается через `ON CONFLICT DO NOTHING` и возвращает существующую строку
  с отпечатком байт в байт: перехват 23505 переводил бы транзакцию потребителя
  в aborted, и законный повтор ронял бы её бизнес-факт. `Claim` — CTE с
  `FOR UPDATE SKIP LOCKED`, аренда до `now+lease` и `Reclaimed` по прежнему
  статусу (тест гонки на четырёх воркерах); `Finish` стирает тему, тела и
  заголовки тем же UPDATE и требует `status = 'sending'` — ноль строк даёт
  `mail.ErrUnavailable`. Ошибки Postgres сворачиваются до SQLSTATE и Message:
  в `pgconn.PgError.Detail` лежит «Failing row contains (…)» со всей строкой,
  включая тело письма. Интеграционные тесты на Postgres 16 (testcontainers,
  своя схема на тест, пропуск по `-short`).
- Паники `mail.NewService` называют поле `Config` и правило
  («Config.Lease must be longer than Config.SendTimeout»): ошибка конфигурации
  читается без исходников пакета. Логика проверок не менялась.
- `mailpg.CheckSchema` — проверка таблицы `email_outbox` на старте
  потребителя, без изменений схемы: колонки и типы (`information_schema`),
  CHECK `email_outbox_body_cleared_chk` и `email_outbox_lock_chk`, индексы
  `ux_email_outbox_dedup` (уникальный), `ix_email_outbox_due`,
  `ix_email_outbox_terminal`. Расхождения — одной ошибкой (`errors.Join`),
  первая строка говорит, что делать; лишние колонки потребителя не считаются
  расхождением; сбой каталога — `mail.ErrUnavailable`. `mailpg.Schema` —
  `schema.sql` через `embed` для тех, кто применяет миграции из кода (тест
  держит равенство файлу). Автомиграции в пакете нет и не будет.
- Примеры для pkg.go.dev (`example_test.go`, `example_config_test.go`):
  `ExampleNewService` с рекомендованным `Config`, `ExampleService_Enqueue`
  (inserted → duplicate → `ErrKeyReused`), `ExampleService_Deliver` (sent,
  тело стёрто, `Stats`), `ExampleUnconfigured` (очередь ждёт провайдера). Все с
  проверяемым `// Output:` на двойниках `mailtest`.
- `README.md` — quickstart для потребителя: установка, миграция (копия
  `schema.sql` + `CheckSchema` на старте), проводка `pgxpool → mailpg → sesv2
  → mailotel → NewService` с рекомендованным `Config`, отправка (ключ из
  факта, `NotAfter`, разбор ошибок, путь `Prepare` + `WithTx`), два фоновых
  задания, прод без провайдера, стенд с sesfake, тесты на двойниках, таблицы
  метрик и алертов из ADR-0001. Фрагменты проверены компиляцией.
- `cmd/sesfake/docker-compose.example.yml` — стенд из sesfake (сборка из
  `cmd/sesfake/Dockerfile`, релей в `mailpit:1025`, регион `ru-central1`) и
  Mailpit; порты только на 127.0.0.1, наружу не выставлять.
- `docs/CHECKLIST.md` — чек-лист встраивания из десяти шагов от `go get` до
  алертов; таблица «Жители» корневого README перечисляет подпакеты `mail`.
