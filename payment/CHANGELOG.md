# Changelog — payment

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

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
- Контрактный набор `TestStoreContract`: семь сценариев гоняются и по двойнику
  `paymenttest.MemStore`, и по адаптеру.
- Страж закрытых наборов: тест разбирает исходники и падает, если константа
  объявлена мимо списка `All*`.
- Мутационный прогон gremlins по ядру (без `paymenttest` и `prorate`): убито
  214, выжил 1, эффективность 99.53%. Единственный выживший — недостижимая
  отрицательная половина потолка в `Money.tryAdd`; разбор и причина, по которой
  его не убить, — в `mutants_internal_test.go`.

### Notes
- `paymentpg` не зависит от модуля `postgres`: ADR-0005 разрешает такую
  зависимость только из `_test.go`, а белый список стража импортов для каталога
  даёт лишь `uuid` и `pgx/v5`. Правило `Sanitize` (SQLSTATE и Message без
  `Detail`) и разбор конфликта по имени скопированы в `paymentpg/errors.go`
  вместе с тестами — как это уже сделано в `mailpg`.
- Тестовый стенд адаптера свой, а не `postgres/pgtest`: под `GOWORK=off`
  зависимость на неопубликованный модуль `rebar/postgres` не разрешается.
  `TEST_DATABASE_URL` поддержан — прогону мутантов нужен общий сервер.
- Модуль ещё не внесён в `go.work` (это делает арбитр при слиянии), поэтому
  локальные прогоны идут с `GOWORK=off` — включая `make mutants`.
- Валидация переписана цепочками `if` вместо `switch { case cond: }`: профиль
  покрытия не описывает блоком условие `case` у бестегового switch, и gremlins
  помечал такие мутанты «NOT COVERED», не запуская их вовсе. После правки
  mutator coverage 100% вместо 81%, «NOT COVERED» ноль вместо 50, убито 264
  вместо 214; четыре из вскрывшихся мутантов были настоящими дырами.
