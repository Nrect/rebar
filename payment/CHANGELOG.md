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
- Страж закрытых наборов: тест разбирает исходники и падает, если константа
  объявлена мимо списка `All*`.
- Мутационный прогон gremlins по ядру (без `paymenttest` и `prorate`): убито
  214, выжил 1, эффективность 99.53%. Единственный выживший — недостижимая
  отрицательная половина потолка в `Money.tryAdd`; разбор и причина, по которой
  его не убить, — в `mutants_internal_test.go`.

### Notes
- Модуль ещё не внесён в `go.work` (это делает арбитр при слиянии), поэтому
  локальные прогоны идут с `GOWORK=off` — включая `make mutants`.
