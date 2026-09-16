# Changelog — idem

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- **Ядро ответа на повтор запроса по ключу `Idempotency-Key`
  ([ADR-0012](../docs/adr/0012-inbox-idempotency.md), решения 9–12), первый из
  трёх шагов модуля.** Адаптер Postgres `idempg` и метрики `idemotel` —
  следующие шаги.
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
