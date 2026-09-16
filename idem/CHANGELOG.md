# Changelog — idem

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- **Хранилище Postgres `idempg` — второй из трёх шагов модуля
  ([ADR-0012](../docs/adr/0012-inbox-idempotency.md), решения 2, 13–15, 17).**
  Зависимости `pgx/v5` v5.10.0 и `postgres` v0.2.0 — граница ошибки,
  разрешённая адаптерам хранилища (ADR-0005).
  - `New(pool, cfg, obs)` — тот же `Config` и наблюдатель, что у
    `idemtest.NewMemStore`: паника на nil-пуле, nil-наблюдателе и негодном
    `Config`, `Config` копируется. `WithTx(tx)` своей транзакции не открывает;
    `SetClock` — часы в UTC по умолчанию, паника на nil.
  - `Do(ctx, req, op func(context.Context, pgx.Tx) (idem.Response, error))` —
    одна транзакция: `pg_try_advisory_xact_lock` по ключу из первых восьми байт
    SHA-256 от домена `rebar/idem/lock/v1`, реалма, субъекта и ключа с
    префиксами длины (эталон посчитан на Python, `TestLockKey_GoldenVector`);
    ключ занят — `idem.InFlight()` без ожидания; запись по первичному ключу —
    `idem.Replay`; иначе op, `Config.CheckResponse` и `INSERT … ON CONFLICT ON
    CONSTRAINT ux_idem_records_key DO NOTHING`. Ошибка op — как есть, сбой базы
    — `idem.ErrUnavailable`; наблюдатель — ровно раз на `Do`, кроме отказа
    `CheckRequest`. `UPDATE` в адаптере нет (`TestAdapter_HasNoUpdate`).
  - В пуле любая ошибка — откат. **В `WithTx` любая ошибка `Do`, включая отказ
    `CheckRequest` и панику op, прерывает транзакцию потребителя** запросом
    `DO … RAISE … USING ERRCODE = '25P02'` с текстом `idempg: транзакция
    прервана после отказа`: её `COMMIT` не проходит (решение 2.4, уточнение 1).
  - Вставка упёрлась в запись, легшую мимо блокировки: в пуле решает
    перечитанная запись (её ответ или `ErrKeyReused`), эффект op уходит
    откатом; в `WithTx` — `ErrUnavailable`, потому что успех закоммитил бы
    эффект второй раз.
  - `Purge(ctx, before, limit)` — контракт `idem.Pruner`: строго раньше
    `before` до микросекунды, самые старые первыми, равные моменты — по ключу
    побайтно, как у двойника; непозитивный `limit` — ноль без базы.
  - Схема — `migrations/00001_idem_init.sql` и `Migrations() fs.FS`
    ([ADR-0011](../docs/adr/0011-migrations-in-blocks.md)): `idem_records`,
    первичный ключ `ux_idem_records_key (realm, subject, idem_key)`, CHECK
    `idem_records_{realm,subject,key,operation,fingerprint,status,body}_chk` в
    формах ядра, индекс `ix_idem_records_created`, триггера нет. Накатывает
    раннер проекта со своей таблицей версий: у goose —
    `goose.NewProvider(goose.DialectPostgres, db, idempg.Migrations(),
    goose.WithTableName("idem_schema_version"))`; модуль goose не импортирует.
    `TestSchemaChecks_MirrorCore` гоняет одни значения через ядро и через CHECK
    и сверяет вердикты.
  - `CheckSchema(ctx)` называет каждое расхождение по имени: колонку и её тип,
    первичный ключ и CHECK, индекс и его уникальность; первая строка — что
    делать.
  - Граница ошибки — `postgres.Sanitize`: наружу `*postgres.Error`, а не
    `*pgconn.PgError` с содержимым записи в `Detail`.
  - Тесты на живой базе: `idemtest.RunDoSuite` по адаптеру;
    `TestStore_Do_IsAtomic` — после ошибки op, 5xx, потолка, сбоя фиксации и
    отката транзакции потребителя нет ни записи, ни эффекта;
    `TestStore_Do_Race` — op держит ключ, пока остальные не вернутся с
    `in_flight`, эффект в базе один; `TestStore_WithTx_AbortsOnError` — девять
    путей ошибки. Помощники `pgtest.ApplyUp`, `ApplyDown` и `CheckMigrations`
    ещё не выпущены тегом: их копия — `pgtestcopy_test.go` (`TODO(ADR-0011)`).
- **Ядро ответа на повтор запроса по ключу `Idempotency-Key`
  ([ADR-0012](../docs/adr/0012-inbox-idempotency.md), решения 9–12), первый из
  трёх шагов модуля.** Адаптер Postgres `idempg` — второй шаг (выше), метрики
  `idemotel` — третий.
  - `ParseKey(fieldLines ...string)` — ключ из строк поля: строка Structured
    Fields и голое значение — один ключ; 1–255 байт видимого ASCII без кавычки
    и обратной косой черты; нет строк — `ErrKeyMissing`; несколько строк,
    параметры, список, пробелы и экранирование — `ErrKeyInvalid`. Регистр не
    меняется. `Key` получается только разбором, нулевое значение Do
    отвергает. Фаззер `FuzzParseKey`.
  - `Request` — область (`Scope{Realm, Subject}` принципала `auth`), операция
    из закрытого набора, ключ, метод, путь как пришёл, сырая строка запроса и
    тело. `Request.Fingerprint` — SHA-256 с префиксом длины у операции,
    метода, пути, строки запроса и тела и тегом формата
    `rebar/idem/request/v1`; формат прибит эталоном
    `TestFingerprint_GoldenVector`, посчитанным вне Go.
  - `Config` — `Operations` (`[a-z0-9_.]{1,64}`, без повторов), `Retention`
    (не меньше `MinRetention`, суток), `MaxResponseBytes` (1..`ResponseCeiling`,
    мегабайт); `Validate` — одно правило на все конструкторы.
  - Чистые решения для Do адаптера и двойника: `Config.CheckRequest` (область,
    операция, ключ, метод POST или PATCH), `Replay` (тот же отпечаток — ответ
    из записи с `Replayed`, другой или потерянный — `ErrKeyReused`),
    `Config.CheckResponse` (статус 200–499, тело только с `Content-Type` и не
    у 204 и 304, заголовки — печатный ASCII без пробела по краям, тело с обоими
    заголовками не больше потолка), `InFlight` — `ErrInFlight` с
    `RetryAfter()` на одну секунду.
  - `Response` хранит только статус, `Content-Type`, `Location` и тело;
    `Result` — ответ и признак повтора.
  - `Observer` (обязателен у хранилищ), `LogObserver` и закрытый набор
    `Outcome`: `executed`, `replayed`, `reused`, `in_flight`, `failed`,
    `not_recordable`, `too_large`, `error`.
  - `Pruner` и `Purger` — уборка старше `Retention` пачками по
    `PurgeBatchSize`, не больше `PurgeBatchesPerRun` пачек за `Run`;
    `SetClock`. Отмена между пачками — причина отмены, а не `ErrUnavailable`;
    отмена, пришедшая во время `Purge`, — тоже причина отмены, хотя
    хранилище отдаёт оборванный запрос своим сбоем.
  - Классы ошибок (ADR-0007):

    | Sentinel | Класс | Почему |
    |---|---|---|
    | `ErrKeyMissing`, `ErrKeyInvalid` | 400 `incorrect-input` | заголовок присылает клиент |
    | `ErrKeyReused` | 409 `conflict` | как `payment.ErrIdempotencyKeyReused` |
    | `ErrInFlight` | 409 `conflict` | ответ несёт `Retry-After` через `InFlight` |
    | `ErrUnavailable` | 503 `unavailable` | хранилище не ответило или не закоммитило |
    | `ErrNotRecordable`, `ErrResponseTooLarge`, `ErrInvalidScope`, `ErrInvalidRequest` | нет, `//errs:nokind` | ответ, его размер, область, операцию и метод строит код потребителя |

  - `idemtest.MemStore` — двойник Do и `Pruner`: уникальность (реалм, субъект,
    ключ), отпечаток, исключение по ключу без общего замка (op зовётся без
    замка, параллельный вызов получает `in_flight`, а не ждёт), срок записи,
    моменты как в `timestamptz`, копия тела на записи и выдаче, отменённый
    контекст и `SetErr` в `ErrUnavailable` с причиной, `SetClock`; паника op
    снимает пометку «в работе». `idemtest.Observer`, `idemtest.Clock`.
  - `idemtest.RunDoSuite(t, factory)` — контрактный набор на голом `testing`,
    один на двойник и `idempg`: 17 сценариев, включая детерминированный
    `in_flight` во время op, гонку одного ключа, отмену до и во время op,
    границу уборки внутри микросекунды и копирование `Config`.
  - `idemhttp`: `Key(r)`, `NewRequest(r, scope, op, body)`, `JSON(status, v)`,
    `Write(w, res)` с `Idempotent-Replayed: true` на повторе; ответ об ошибке —
    ответчиком потребителя.
  - Компилируемый пример ручки `example_test.go` и рецепт `README.md`.
