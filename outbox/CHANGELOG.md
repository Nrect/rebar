# Changelog — outbox

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Changed
- **`outboxtest.MemStore` хранит и отдаёт моменты как `timestamptz`: в UTC и с
  точностью до микросекунд, — и с той же точностью сравнивает параметры
  (аренда в `Claim`, `Purge`).** Раньше двойник отдавал время как передали, с
  наносекундами и зоной: сравнение меток у потребителя было зелёным на
  двойнике и красным на базе, а наносекунда после конца аренды отдавала
  второму воркеру строку, которую база ещё держит. Контракт записан у
  `outbox.Store`; `outboxtest.RunStoreSuite` получил четыре сценария — моменты
  вставки, `Stats` и `Claim`; моменты исходов и `Redrive`; аренда и `Purge` на
  границе одной микросекунды — и гоняет их и по двойнику, и по `outboxpg`.
  Ломающее для теста потребителя, который сравнивал момент строки двойника
  голым `==` или `assert.Equal` с часами в чужой зоне или с наносекундами, — на
  базе такой тест был красным всегда.
- `doc.go`: на пути вставки класс ошибки принадлежит потребителю — вставка
  идёт его хендлом в его транзакции, ядро ошибку не видит и не заворачивает.
- **Класс ошибки у sentinel ([ADR-0007](../docs/adr/0007-error-kind.md)).**
  Потребителю больше не нужна таблица перевода ошибок `outbox`: `errs.KindOf`
  и `httperr` находят класс на самой sentinel. Страж
  `errstest.EveryErrorHasKind` стоит в корне модуля (`sentinels_test.go`);
  двойники `outboxtest` из него исключены — их ошибки только причины, класс
  несёт обёртка ядра, и это держит `TestPortFailuresReachCallerAsUnavailable`.
  Вставку ядро не делает: её зовёт потребитель адаптером в своей транзакции,
  поэтому класс ошибки на этом пути принадлежит ему (`doc.go`); `outboxpg`
  заворачивает сбой в `ErrUnavailable` сам.

  | Sentinel | Класс | Почему |
  |---|---|---|
  | `ErrKeyReused` | 409 `conflict` | тот же `(Kind, DedupKey)` на другое сообщение |
  | `ErrClaimLost` | 409 `conflict` | строку держит другой токен аренды, состояние не менялось |
  | `ErrUnavailable` | 503 `unavailable` | сбой хранилища, повтор осмыслен |
  | `ErrInvalidMessage`, `ErrBadKind`, `ErrKeyInvalid` | нет, `//errs:nokind` | сообщение, тип и ключ строит код потребителя из факта: негодные — дефект, то есть 500 |
  | `ErrSkip` | нет, `//errs:nokind` | не отказ, а сигнал хендлера воркеру: строка закрывается как done |

- **Ломающее для кода, который сравнивал тексты sentinel, присваивал их,
  различал `outbox.Backoff` и `mail.Backoff` как типы или звал
  `SetClock(nil)`.** Замена:

  | Было | Стало |
  |---|---|
  | тип sentinel с классом — `error` | `errs.KindError`; `errors.Is` и `==` работают как прежде |
  | `message is invalid`, `unknown message kind`, `dedup key …`, `claim was lost: …` | тот же текст с префиксом `outbox: ` |
  | `outbox operation could not be completed` | `outbox: operation could not be completed` |
  | `outbox.Backoff` — свой тип пакета | `outbox.Backoff = retry.Backoff` (псевдоним): литерал `outbox.Backoff{Base, Max}` и `Delay` работают как прежде; ломается код, различавший `outbox.Backoff` и `mail.Backoff` как разные типы (оба в одном type switch, `%T`, reflect) |
  | `Producer.SetClock(nil)`, `Worker.SetClock(nil)` принимались и падали разыменованием при первом обращении к часам | паника `outbox.Producer.SetClock: now must not be nil` и `outbox.Worker.SetClock: now must not be nil` |

  Префикс не косметика: `KindError` равны по классу и тексту, и без него
  `outbox.ErrKeyReused` совпала бы через `errors.Is` с `mail.ErrKeyReused`.
  Модуль требует `github.com/nrect/rebar/kit v0.2.0`.
- `Backoff` берётся из `kit/retry`: копия экспоненты с джиттером удалена.
  Поведение то же — сверено построчно: тело и структура копии совпадали с
  `kit/retry`. Расходился только комментарий: он называл формулу
  `Base·2^attempt`, а считала копия, как и `kit`, `Base·2^(attempt−1)`. Тесты
  границ backoff остались в модуле и гоняют `kit` через псевдоним.
- **API двойников меняется ломающе: настройка `outboxtest.RecordingHandler` и
  `outboxtest.MemStore` — методы, а не публичные поля.** Двойники читали поля
  под своим мьютексом, а тест писал их мимо него. Пока поле ставится до первого
  вызова, гонки нет; но тест потребителя, у которого хендлер зовёт воркер в
  другой горутине (или ручка живого HTTP-сервера), пишет поле, пока двойник его
  читает, — и `-race` краснеет у потребителя. `FailFor` и `PanicFor` двойник к
  тому же сам декрементирует под замком, и запись мимо замка роняет процесс
  `fatal error: concurrent map writes` — упавший прогон, а не красный тест.
  Теперь настройка правится под тем же замком, хуки зовутся вне его и вправе
  звать сам двойник ([CONVENTIONS §3](../CONVENTIONS.md#3-двойники)). Замена:

  | Было | Стало |
  |---|---|
  | `h.FailFor[key] = n` | `h.FailFor(key, n)` |
  | `h.PermanentFor[key] = true` | `h.PermanentFor(key)` |
  | `h.ThrottleFor[key] = after` | `h.ThrottleFor(key, after)` |
  | `h.SkipFor[key] = true` | `h.SkipFor(key)` |
  | `h.PanicFor[key] = n` | `h.PanicFor(key, n)` |
  | `h.Hook = hook` | `h.SetHook(hook)` |
  | `store.Err = err` | `store.SetErr(err)` |
  | `store.FinishErr = err` | `store.SetFinishErr(err)` |
  | `store.AfterHandle = hook` | `store.SetAfterHandle(hook)` |

  Счётчики снимаются нулём, ошибки и хуки — `nil`. Замены `delete` по картам
  `PermanentFor`, `ThrottleFor` и `SkipFor` нет: поведение, меняющееся по ходу
  теста, задаётся хуком — им же задаётся отказ, когда идентификатор агрегата
  рождается внутри боевого кода и тест его не знает.

## [0.1.0] — 2026-09-10

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
  Ошибки — через `postgres.Sanitize`: `PgError.Detail` с payload наружу не
  уходит, а классификация по SQLSTATE и имени ограничения границу переживает.
- Пакетная `outboxpg.Enqueue(ctx, tx, env)` — путь потребителя: вставка и
  сверка отпечатка одним вызовом. Сырой `Store.Enqueue` остаётся воркеру,
  контрактным тестам и своей обёртке. `CheckSchema` сверяет колонки,
  именованные CHECK и индексы, возвращает все расхождения одной ошибкой и
  схему НЕ применяет.
- `outboxtest.Enqueue` — тот же вызов поверх двойника: без него тест
  потребителя писал бы два шага там, где прод пишет один, и расходился бы с
  ним на «громкой идемпотентности».
- `outboxtest.RunStoreSuite(t, factory)` — контрактный набор порта `Store` из
  двенадцати сценариев, переиспользуемый: его гоняет и двойник, и `outboxpg`, и
  тот, кто напишет свою реализацию. Набору не нужны ни Docker, ни управляемые
  часы — времена в порту параметры, и все моменты набор задаёт сам. Пакету
  положены только stdlib и `uuid`, поэтому набор написан на `testing`, без
  testify.
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
  Плюс `outboxtest.RunStoreSuite`, который гоняется в одном бинаре и по
  `outboxtest.MemStore`, и по `outboxpg.Store`: двойник и адаптер не имеют
  права разойтись.
- Тест на утечку содержимого строки проверяет НЕДОСЯГАЕМОСТЬ `*pgconn.PgError`
  через `errors.As`, а не только текст ошибки: `Detail` не входит в
  `PgError.Error()`, поэтому проверка «секрета нет в тексте» зелена и на
  адаптере без границы. Тест сначала доказывает, что утекать есть чему (сырая
  ошибка того же INSERT несёт payload в `Detail`), и падает, если границу
  снять.
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
