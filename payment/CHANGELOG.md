# Changelog — payment

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

## [0.2.0] — 2026-09-10

Минорный номер, хотя изменения ломающие: в v0 семвер ломающих гарантий не даёт.
Что сломается при обновлении и что с этим делать — в разделе `Changed`:
`NewService` получил обязательного наблюдателя, у двойников `MemProvider` и
`MemStore` не осталось публичных полей, окончательные отказы провайдера больше
не 503 — без правил на `ErrProviderRejected` и `ErrUnsupported` у потребителя
они станут 500.

### Added
- Подпакет `paymentotel` — наблюдаемость на OpenTelemetry, meter
  `rebar.payment`. `Wrap(provider, meter)` — декоратор порта `Provider` со
  счётчиком `payment_provider_calls{provider,type,result}`: `provider` —
  `Name()` провайдера (форма `[a-z0-9_]{1,32}`, её проверяет `NewService`;
  два провайдера на одном метре не сливаются в один ряд), `type` — закрытый
  набор `AllCallTypes` (шесть методов порта; `Name` пробрасывается как есть и
  не считается), `result` — `ok` / `rejected` / `error`. Отказ и молчание
  провайдера не сводятся, и граница между ними та же, что у сервиса:
  `rejected` — ровно те ответы, на которые сервис не отдаёт `ErrUnavailable`
  (`ErrProviderRejected`, `ErrUnsupported`, `Status == EventFailed` у
  `CreatePayment`; у `ParseWebhook` — `ErrInvalidSignature` и явный
  `ErrMalformedEvent`), всё прочее, включая неизвестную ошибку, — `error`.
  `NewGauges(meter)` — `payment_intents_stuck`
  и `payment_drift{kind}` по снимку, который потребитель кладёт в `Set` из
  отдельной задачи планировщика (`CountStuckPending` и `Drift` одним заходом;
  не на scrape и не в задаче сверки); записи расхождений не хранятся, род
  вне `AllDriftKinds` идёт рядом без метки. otel стал прямой зависимостью
  модуля; ядро его по-прежнему не импортирует, а белый список стража для
  `paymentotel` сужен до `otel/metric` и `otel/attribute`.
- Порт `Observer` (`Outcome(ctx, op, reason)`) и закрытый набор `Op` с
  `AllOps` (`start`, `webhook`, `capture`, `cancel`, `refund`, `reconcile`):
  сервис отдаёт наблюдателю исход КАЖДОЙ публичной операции, на успехе и на
  ошибке, с тем же `Reason`, что вернул вызывающему. `LogObserver(l)` — явный
  выбор того, кому метрики не нужны: тревоги с порогом 1 (`status_conflict`,
  `amount_mismatch`) в Error, остальное в Debug. Двойник
  `paymenttest.Observer`. `paymentotel.NewObserver(meter)` — счётчик
  `payments_total{op,reason}`; все пары `AllOps × AllReasons` заводятся нулём
  при сборке, иначе первый инкремент ряда не виден `increase()`, а у двух
  денежных алертов порог 1. Обещание `payment/doc.go` про `payments_total`
  стало правдой.

### Changed
- **Меняется HTTP-статус окончательных отказов провайдера у потребителя.**
  Раньше `ErrProviderRejected` и `ErrUnsupported` из `Start`, `Capture`,
  `Cancel`, `Refund` и `Reconcile` приезжали под `ErrUnavailable`, и таблица
  «ошибка → HTTP», проверяющая `ErrUnavailable` первой, отдавала на них 503.
  Теперь обёртки нет: `ErrProviderRejected` получит ваше правило, если оно
  есть, а `ErrUnsupported` без правила — умолчание, то есть 500, что хуже
  прежнего 503. Заведите правила до обновления: `ErrProviderRejected` — 409
  или 422, `ErrUnsupported` — 501 (ретраем не чинится и не ваш баг), иначе
  окончательный отказ станет 500. `Reason` для `ErrProviderRejected` от
  адаптера — теперь `provider_rejected`, а не `provider_error`. Вебхук
  меняется в обратную сторону: неклассифицированная ошибка `ParseWebhook` —
  503 вместо 400, и провайдер повторит уведомление, которое раньше считал
  доставленным.
- **Ломающее:** `NewService(store, provider, obs, cfg)` — наблюдатель стал
  обязательной зависимостью, nil — паника. Не поле `Config` и не
  `SetObserver`: забытый вызов дал бы молчание ровно на денежных алертах.
- **Ломающее для тестов:** у `paymenttest.MemProvider` не осталось
  публичных полей. Ручки (`CreateErr`, `GetErr`, `CaptureErr`, `CancelErr`,
  `RefundErr`, `ParseErr`, `NoHolds`, `BadSignature`, `NoPaymentID`,
  `Result`, `RefundEcho`, `RejectFor`, `FailFor`, `CreateHook`) методы
  двойника читали под своим мьютексом, а тест писал мимо него: у
  потребителя, который гоняет двойник через живой HTTP-сервер, это гонка под
  `-race`, и краснела бы она у него. Теперь ручки — методами под тем же
  замком (`SetCreateErr` … `SetParseErr`, `SetNoHolds`, `SetBadSignature`,
  `SetNoPaymentID`, `SetResult`, `SetRefundEcho`, `RejectReference`,
  `FailReference`, `SetCreateHook`), записанные запросы — копиями
  (`Created()`, `Captures()`, `Cancels()`, `Refunds()`). Для HTTP-теста,
  который ссылку заказа заранее не знает, — `RejectNext()`: отказ следующему
  новому платежу. `CreateHook` зовётся вне замка: хук вправе трогать сам
  провайдер, под замком это была взаимная блокировка.
- **Ломающее для тестов:** у `paymenttest.MemStore` тоже не осталось
  публичных полей — дефект тот же, что у `MemProvider`. Ручки — методами под
  замком: `SetErr`, `SetRaceOnce`, `SetRefundTooLargeOnce`,
  `SetDriftRecords`, `SetOnSettled`, `SetOnRefunded`; для состояний, до
  которых сервис не доводит, — `SeedKey` (ключ в индексе без строки) и
  `ClearEntries` (книга стёрта мимо append-only); чтение — копиями
  (`Entries()`, `LastApplySeq()`, прежние `EntriesOf`, `Deliveries`,
  `CallCount`). Хуки по-прежнему зовутся ПОД замком двойника — как у
  адаптера внутри транзакции: отпусти двойник замок, и параллельный вызов
  вклинился бы между предикатом и применением. Отсюда контракт: хук двойник
  не трогает.

### Fixed
- Вебхук: неклассифицированная ошибка `ParseWebhook` уходила в
  `ReasonMalformedEvent`, то есть в 400. Сырой `context.DeadlineExceeded`
  проверочного чтения у адаптера, забывшего его обернуть, становился 400:
  провайдер считал уведомление доставленным и больше не приходил — оплата
  терялась навсегда. Умолчание перевёрнуто: 400 — только по явному
  `ErrMalformedEvent` или `ErrInvalidSignature`, всё неклассифицированное —
  `ReasonProviderError` под `ErrUnavailable`, то есть 503. Контракт
  `ParseWebhook` в `ports.go` говорит это прямо.
- Окончательный отказ провайдера отдавался как «попробуйте позже». Все пять
  мест вызова (`Start`, `Capture`, `Cancel`, `Refund`, `Reconcile`)
  заворачивали в `ErrUnavailable` и `ErrUnsupported`, и `ErrProviderRejected`:
  потребитель с таблицей «ошибка → HTTP» отвечал на них 503, и клиент
  повторял вечно, а `ErrProviderRejected` вдобавок получал причину
  `provider_error`. Теперь одна точка `providerError`: `ErrUnavailable` —
  только «ответа нет», окончательные классы идут своим `%w` с причинами
  `unsupported` и `provider_rejected`. Определённое «нет» от `CreatePayment` —
  `ErrProviderRejected` или `ErrUnsupported` — закрывает попытку так же, как
  `Status == EventFailed`: прийти по ней нечему, а открытой её держит только
  «ответа нет». Раньше `ErrUnsupported` оставлял намерение в `created`, и
  сверка звала `CreatePayment` до самого TTL. Причина закрытия — по классу:
  `provider_rejected` смотрит поддержка, `unsupported` чинит тот, кто
  настраивает интеграцию. Сама причина в намерении не хранится — `failed`
  один на оба класса, — поэтому повтор ключа по закрытой так попытке отдаёт
  общий окончательный отказ `provider_rejected`: не 503 и без второго похода
  к провайдеру. Контракт `Provider` в `ports.go` называет законные классы по
  методам.

## [0.1.0] — 2026-09-10

### Added
- Каркас пакета: `Intent` со снапшотом состава (`OrderItem`), `LedgerEntry`,
  `Event`, `Confirmation`, закрытые наборы `Status` (с `authorized` — холд —
  с первого дня), `Reason`, `EventType`, `LedgerKind`, `ConfirmationType`,
  `ApplyOutcome`, `DriftKind`; `Money` с потолком `MaxMoneyMinor` и паникой на
  смешении валют; `Config` с panic-валидацией и `SetClock`; страж импортов с
  белым списком по каталогам (включая ещё не написанные `paymentpg`,
  `yookassa`, `paymentotel`, `cmd/psfake`).
- Порты `Store` и `Provider` с полными контрактами: атомарность `ApplyEvent`
  (порядок блокировок intent → events → ledger → хук потребителя), дедуп
  событий по `(provider, provider_event_id)`, `UNIQUE (payer_id,
  idempotency_key)`, частичный уникальный индекс «одно живое намерение на
  `Reference`», потолок `Σrefund ≤ capture` под блокировкой и триггером,
  `UNIQUE (intent_id, idempotency_key)` в книге. Контракт хука адаптера
  (`Settler`) описан в `ports.go` — по нему пишется `paymentpg`.
- Покупка: `Service.Start` (нормализация ключа в одной точке, отпечаток
  параметров с дайджестом состава, проверка чека ДО похода к провайдеру,
  дозавершение брошенной попытки, протухание по TTL, разбор проигранной гонки
  за ключ, отказ по занятой ссылке заказа).
- Зачисление: `Service.HandleWebhook` и `Service.Reconcile` одним путём
  применения; контракт ошибок вебхука (200 / 400 подпись / 400 разбор / 503
  хранилище), классификация исходов в `Reason`.
- Двухстадийная оплата: `Service.Capture` и `Service.Cancel`, порт
  `Provider.Capture`/`Cancel`, `ErrUnsupported` у адаптеров без холдов.
  Частичного списания нет: сумма заморожена в намерении.
- Возврат: `Service.Refund` с явной суммой и частичными возвратами
  (`0 < amount ≤ нетто`), производный ключ провайдера, сверка эха суммы,
  громкий отказ, если книга не приняла уже выполненный провайдером возврат.
- Сверка: `Service.StalePending` с курсором, `CountStuckPending`, `Drift`,
  плюс `Reconciler` с `Run(ctx) (int, error)` — сигнатурой `scheduler.Job.Run`.
- Чтение через домен: `Service.IntentByID`, `Service.IntentByKey` и
  `Service.Ledger`. `IntentByKey` нормализует ключ сам, как `Start`: две точки
  нормализации — это два ключа, и вызывающий, забывший нормализовать, получил
  бы «намерения нет» на существующем. Без этих методов потребителю приходилось
  держать `Store` и читать мимо сервиса.
- Чек 54-ФЗ: `Receipt` с `Customer{Email, Phone}` (достаточно одного),
  необязательными `MarkCode` и `Measure`; `CheckReceipt` — одна функция на все
  точки вызова.
- Подпакет `prorate` — чистая пропорция за неиспользованный срок (`Prorate`,
  `Split`, `Sum`, `UsedUnits`) без типов ядра и без зависимостей.
- Двойники `paymenttest`: `MemStore` (оба ограничения схемы, дедуп событий,
  потолок возвратов, хуки `OnSettled`/`OnRefunded`, `Err`, `RaceOnce`,
  `RefundTooLargeOnce`), `MemProvider` (идемпотентность по ключу,
  детерминированный `ProviderEventID`, `NoHolds`, `RejectFor`, `FailFor`,
  `RefundEcho`), `Clock`.
- Адаптер `paymentpg` на pgx/v5: `New(pool, Options{Settler})`, `WithTx(tx)`,
  `CheckSchema(ctx)`; `schema.sql` с маркерами goose — таблицы
  `payment_intents`, `payment_intent_items`, `payment_events` (inbox со
  счётчиком доставок) и append-only `payment_ledger`.
- Схема как контракт: `ux_payment_intents_key` (полный `UNIQUE (payer_id,
  idempotency_key)` — провалившаяся попытка ключ не освобождает),
  `ux_payment_intents_live_reference` (частичный, «одно живое намерение на
  заказ»), `ux_payment_events_dedup`, `ux_payment_ledger_capture` (одно
  зачисление на намерение), `ux_payment_ledger_key`. Адаптер различает их ПО
  ИМЕНИ: у первых двух исходы противоположные (`ErrIdempotencyRace` против
  `ErrReferenceBusy`).
- Книгу держит база: триггеры `payment_ledger_immutable_trg`,
  `payment_ledger_no_truncate_trg` и `payment_ledger_refund_cap_trg`, все
  `ENABLE ALWAYS`; они представляются именами ограничений
  (`payment_ledger_immutable`, `payment_ledger_refund_cap`,
  `payment_ledger_refund_currency`), поэтому разбираются так же, как индексы.
  `CheckSchema` проверяет в том числе режим `ENABLE ALWAYS`.
- Хук потребителя `paymentpg.Settler` (`OnSettled`/`OnRefunded` с `pgx.Tx`):
  зовётся после записи книги и до commit, ошибка откатывает всё, включая строку
  дедупа события.
- `ApplyEvent` проверяет форму запроса до первой записи: книга непуста тогда и
  только тогда, когда цель — `succeeded`, запись это зачисление и принадлежит
  тому же намерению (каждое расхождение неисправимо: книга append-only).
- Контрактный набор `paymenttest.RunStoreSuite(t, newStore)`: десять сценариев
  на голом `testing` (двойники обязаны собираться у потребителя без тестовых
  зависимостей). Гоняется по `MemStore` и по `paymentpg` в одном бинаре —
  расхождение реализаций видно сразу, а не после выката.
- Страж закрытых наборов: тест разбирает исходники и падает, если константа
  объявлена мимо списка `All*`.
- Мутационный прогон gremlins по ядру (без `paymenttest`, `prorate` и
  адаптера `paymentpg`): убито 268, выжил 1, эффективность 99.63%, mutator
  coverage 100%. Единственный выживший — недостижимая отрицательная половина
  потолка в `Money.tryAdd`; разбор и причина, по которой его не убить, — в
  `mutants_internal_test.go`. Цифры пересняты после появления читающих методов
  и после того, как адаптер уехал в исключения (CONVENTIONS §5: его держат
  интеграционные тесты).

### Changed
- Интеграционные тесты `paymentpg` переехали на общий стенд `postgres/pgtest`:
  свои `startPostgres`, `adminPool`, `newPool`, разбор `TEST_DATABASE_URL` и
  уборка схемы сняты — всё это уже есть в стенде, и три копии одного стенда
  расходились бы по одной. `pgtest` вдобавок подметает базы, брошенные
  прерванным прогоном. `testcontainers-go` перестала быть прямой зависимостью
  модуля.
- Своя копия разбора goose-секций снята вместе с её регрессным тестом на
  `StatementBegin`: секцию `Up` отдаёт `pgtest.GooseUp`, маркеры —
  `pgtest.GooseUpMarker` и `pgtest.GooseDownMarker`. Правда о строке директивы
  стала одна, и сторожит её `pgtest`.

### Fixed
- `paymenttest.MemStore` ставил `SettledAt = req.Now`, а контракт порта требует
  `Event.OccurredAt`, зажатое в `[intent.CreatedAt, req.Now]`: событие может
  доехать через час после списания, и выручка «за январь» уезжала в феврале
  у прода, но не у двойника — расхождение ровно на колонке, по которой режут
  выручку.
- `MemStore.StalePending` и `MemStore.Drift` паниковали на непозитивном размере
  пачки (`queue[:min(limit, len(queue))]`), тогда как адаптер отвечает ошибкой.
  Контракт порта дополнен: непозитивный `limit` — ошибка программиста и
  `ErrBadTransition`, а не пустая пачка (она тихо остановила бы сверку) и не
  паника.

### Notes
- `paymentpg` зависит от модуля `postgres` (ADR-0005, третья межмодульная
  зависимость): граница ошибки — общая `postgres.Sanitize`, а не своя копия.
  Локальным остался только `raisedBy`: `postgres.IsUniqueViolation` закрывает
  23505, а инварианты книги держат триггеры и приезжают как 23514 с тем же
  полем имени.
- Тестовый стенд адаптера — общий `postgres/pgtest` (третья межмодульная
  зависимость ADR-0005, только из `_test.go`). `TEST_DATABASE_URL` он умеет
  сам: прогону мутантов нужен общий сервер, иначе каждый мутант поднимает свой
  контейнер.
- Модуль ещё не внесён в `go.work` (это делает арбитр при слиянии), поэтому
  локальные прогоны идут с `GOWORK=off` — включая `make mutants`.
- Валидация переписана цепочками `if` вместо `switch { case cond: }`: профиль
  покрытия не описывает блоком условие `case` у бестегового switch, и gremlins
  помечал такие мутанты «NOT COVERED», не запуская их вовсе. После правки
  mutator coverage 100% вместо 81%, «NOT COVERED» ноль вместо 50, убито 264
  вместо 214; четыре из вскрывшихся мутантов были настоящими дырами.
