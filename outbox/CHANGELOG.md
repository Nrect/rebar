# Changelog — outbox

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- Каркас пакета: типы (`Kind`, `Message`, `Envelope`, `Delivery`, закрытые
  наборы `Status`, `FailReason`, `FinishOutcome`, `EnqueueOutcome` с `All*` и
  guard-тестом), `Config` с panic-валидацией, `Backoff` с полным джиттером и
  защитой от переполнения сдвига, `NormalizeKey`, отпечаток сообщения (sha256
  с префиксами длины по kind, payload, агрегату, версии схемы и отсортированным
  заголовкам), страж импортов с картой на `outboxtest`, `outboxpg` и
  `outboxotel`.
- Порт `Store` (`Enqueue`, `Claim`, `Finish`, `Stats`, `ListFailed`, `Redrive`,
  `Purge`) — только примитивы, `uuid`, `time` и типы пакета; все времена
  параметром. Требования к реализации выписаны в контракте: уникальность
  `(kind, dedup_key) WHERE dedup_key <> ''` и разбор конфликта по ИМЕНИ
  ограничения без падения транзакции потребителя; `Claim` фильтрует по списку
  типов, берёт `pending` по сроку и `processing` с истёкшей арендой (ставя
  `Reclaimed`), увеличивает `attempts`, пишет `claim_token` и `locked_until`;
  `Finish` условный по `claim_token` — ноль строк даёт `ErrClaimLost` и не
  меняет состояние; `payload` не стирается ни в одном исходе, включая `failed`;
  `Purge` не трогает `failed`.
- `Producer`: чистый `Prepare` (валидация, нормализация ключа, отпечаток,
  идентификатор, времена в UTC) и `SetClock`. Вставки через пул у пакета нет —
  конверт кладёт адаптер в транзакции бизнес-факта. `CheckDuplicate` — сверка
  повтора по отпечатку: тот же ключ на другое сообщение даёт `ErrKeyReused`.
- `Registry` с паникой на дубле, пустом типе и nil-хендлере; `Handler`,
  `HandlerFunc`, `ErrSkip` (check-at-send: предикат не подтвердился, эффекта
  нет).
- Классы ошибок читаются структурно, по методам `Permanent() bool` и
  `RetryAfter() (time.Duration, bool)`: обёртки `Permanent`, `Throttled` и
  предикаты `IsPermanent`, `RetryAfterOf`. Импорта чужого пакета ради
  `errors.As` не требуется (ADR-0005).
- `Worker`: `Drain(ctx) (int, error)` в сигнатуре `scheduler.Job.Run` — пачка
  под арендой с новым токеном, хендлер под `HandlerTimeout` и `recover`,
  исходы done / skipped / retry / failed(permanent|exhausted) / expired /
  released. `NotAfter` наступил — `expired` без вызова хендлера; названный
  срок повтора уважается с потолком `Backoff.Max` и не позже `NotAfter` и не
  жжёт лимит попыток; паника хендлера — временная ошибка с пометкой `panic:`
  в `LastError`; отмена `ctx` до старта хендлера возвращает остаток пачки в
  `pending` без потраченной попытки (`released`, по ОТВЯЗАННОМУ от отмены
  контексту — иначе освобождение было бы тихим no-op); сбой `Finish` и
  `ErrClaimLost` останавливают пачку с `ErrUnavailable`. Плюс `Purge`,
  `Stats`, `ListFailed`, `Redrive`, `Kinds`, `SetClock`. `NewWorker`
  отказывается собираться, если типы реестра не подмножество `Config.Kinds`
  или реестр пуст.
- Строки с типом без хендлера не забираются вовсе (`Claim` фильтрует по
  реестру) и видны как `Stats.Unhandled`: при выкате новой версии старый
  инстанс не утопит в dead-letter то, что умеет только новая.
- `Stats.OldestDueAge` считает возраст самой старой строки, чей срок УЖЕ
  наступил: отложенные и арендованные не считаются, иначе гейдж горел бы от
  штатной работы.
- Двойники `outboxtest`: `MemStore` (уникальность `(Kind, DedupKey)`, аренда со
  SKIP-LOCKED-семантикой и `Reclaimed`, fencing по токену, сохранение payload,
  `Err`/`FinishErr`, хук `AfterHandle` для имитации убитого процесса между
  `Handle` и `Finish`, снимки `Rows`/`Get` копиями, отказ по отменённому
  контексту), `RecordingHandler` (`FailFor`, `PermanentFor`, `ThrottleFor`,
  `SkipFor`, `PanicFor` по `AggregateID` или `Kind`, `Hook`, `Handled`),
  `Clock`. Ошибки двойников (`ErrIDReused`, `ErrHandlerFailed`) отличимы от
  доменных.
- Тесты ядра на управляемых часах: таблица решений `Drain`, гонка двух воркеров
  на 200 строках («каждая ровно один раз»), `TestDrain_CrashBetweenHandleAndFinish`
  (эффект случился, исход не записан — после аренды повтор с `Reclaimed`),
  потеря аренды, остановка пачки, границы `Prepare`. Мутационный прогон
  (gremlins): 122 убито, 4 выживших разобраны в `mutants_internal_test.go` как
  эквивалентные.
