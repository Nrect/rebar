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
- Два следствия политики записаны в `doc.go` вслух, а не выводятся из кода:
  повтор по названному провайдером сроку не расходует `MaxAttempts`, поэтому
  строка без `NotAfter` под вечным троттлингом ждёт неограниченно (выбор в
  пользу «не потерять событие», детектор — гейдж возраста); `Stats.Unhandled`
  кратко ненулевой при выкате новой версии, поэтому алерт по нему нужен с
  выдержкой.
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
- Адаптер `outboxpg`: `Store` на pgx/v5 с `New(pool)` и `WithTx(tx)`,
  `schema.sql` с goose-маркерами и обеими сторонами. Таблица
  `outbox_messages`, именованные ограничения `outbox_messages_claim_chk`
  (аренда существует ровно у `processing`) и `outbox_messages_fail_chk`
  (причина непуста ровно у `failed`), частичный уникальный индекс
  `ux_outbox_messages_dedup (kind, dedup_key) WHERE dedup_key <> ''` и
  индексы `ix_outbox_messages_due`, `_terminal`, `_failed`, `_aggregate` —
  имена контрактные. Без имени схемы, без FK на таблицы потребителя, без
  `DEFAULT now()`.
- Конфликт дедупа разбирается `ON CONFLICT (kind, dedup_key) WHERE dedup_key
  <> '' DO NOTHING` плюс `SELECT`, а не перехватом `23505`: ошибка Postgres
  перевела бы транзакцию бизнес-факта в aborted, и законный повтор события
  ронял бы сам факт. `ON CONSTRAINT` здесь неприменим — индекс частичный, а
  эта форма умеет только ограничения.
- `Claim` — CTE с `FOR UPDATE SKIP LOCKED`: возвращает прежний статус и по
  нему ставит `Reclaimed`, считает попытку при захвате, фильтрует по списку
  типов. `Finish` условный по `claim_token` — ноль строк даёт `ErrClaimLost`
  и не меняет ничего. `Stats` считает возраст по строкам, чей срок уже
  наступил, и `Unhandled` по переданному списку типов; `Purge` не трогает
  `failed`; `Redrive` работает только из `failed` и сохраняет `last_error`.
  Ошибки — через границу, оставляющую SQLSTATE и Message: `PgError.Detail`
  с payload наружу не уходит.
- Пакетная `outboxpg.Enqueue(ctx, tx, env)` — путь потребителя: вставка и
  сверка отпечатка одним вызовом. Сырой `Store.Enqueue` остаётся воркеру,
  контрактным тестам и своей обёртке. `CheckSchema` сверяет колонки,
  именованные CHECK и индексы, возвращает все расхождения одной ошибкой и
  схему НЕ применяет.
- `outboxtest.Enqueue` — тот же вызов поверх двойника: без него тест
  потребителя писал бы два шага там, где прод пишет один, и расходился бы с
  ним на «громкой идемпотентности».
- Адаптер `outboxotel`: декоратор реестра со счётчиком
  `outbox_handled{kind,result}` (закрытый набор из семи исходов, guard-тест),
  гистограммой `outbox_handle_duration{kind}` и span'ом доставки, связанным с
  породившим запросом ССЫЛКОЙ на `traceparent` из заголовков. `Gauges.Set`
  отдаёт пять величин из одного снимка `Stats` одним коллбэком; в базу гейджи
  не ходят. Паника считается исходом и летит дальше — recover в декораторе
  ослепил бы `Drain`; ошибка next уходит без изменений, иначе классы,
  читаемые по методам, не пережили бы обёртку.
- Тесты адаптера: `TestEnqueue_WithTx_IsAtomic` (откат уносит и строку, и
  ключ дедупа), `TestEnqueue_DuplicateDoesNotAbortTx`,
  `TestStore_Claim_FourWorkersRace` (каждая строка ровно одному),
  `TestStore_Finish_StaleTokenAffectsNoRows`,
  `TestStore_Claim_SkipsUnknownKinds`, `TestStore_Redrive_OnlyFromFailed`,
  `TestStore_Purge_KeepsFailed`,
  `TestStore_Stats_OldestDueIgnoresDeferredAndClaimed`, `TestCheckSchema_*`.
  Плюс контрактный набор из двенадцати сценариев, который гоняется в одном
  бинаре и по `outboxtest.MemStore`, и по `outboxpg.Store`: двойник и адаптер
  не имеют права разойтись.
- Известное свойство колонки, сказанное вслух: `payload` лежит в `JSONB` и
  переживает круг через базу как значение, а не как байты (нормализуются
  пробелы и порядок ключей). Тождество сообщения от этого не страдает —
  отпечаток лежит отдельной колонкой `BYTEA` и возвращается байт в байт;
  именно поэтому ядро хранит его, а не пересчитывает по прочитанному payload.
- Тесты ядра на управляемых часах: таблица решений `Drain`, гонка двух воркеров
  на 200 строках («каждая ровно один раз»), `TestDrain_CrashBetweenHandleAndFinish`
  (эффект случился, исход не записан — после аренды повтор с `Reclaimed`),
  потеря аренды, остановка пачки, границы `Prepare`. Мутационный прогон
  (gremlins): 122 убито, 4 выживших разобраны в `mutants_internal_test.go` как
  эквивалентные.
