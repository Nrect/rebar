# Changelog — kit

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

`kit` — единственный многопакетный модуль тулкита: у его пакетов нет внешних
зависимостей, поэтому отдельный модуль на каждый не окупался бы. Появление
внешней зависимости у пакета — повод вынести его в свой модуль, а не
расширить белый список стража (см. `doc.go`).

## Unreleased

### Added
- Каркас модуля: `go.mod` (go 1.25.0, toolchain go1.26.6; testify только для
  тестов), корневой `doc.go` с правилом про внешние зависимости,
  `importguard_test.go` — страж строже, чем в `mail`: белый список КАЖДОГО
  каталога пуст, непустой список сам роняет тест. В карте заранее объявлены
  каталоги `secrets`, `ratelimit`, `ratelimit/ratelimithttp`, `retry`;
  `testdata` не проверяется (компилятор её не собирает).
- `errs` — ошибка со слагом: `SlugError{Slug, Kind, cause}` (значение, а не
  указатель), закрытый `Kind` из одиннадцати классов с `AllKinds` и
  guard-тестом по исходнику, `New` с паникой на негодном слаге или классе,
  `Error`/`Unwrap`/`Is` (по `Slug`+`Kind`, причина в сравнении не участвует),
  `WithCause` (копия, а не мутация), `KindOf`/`SlugOf`, `TranslateAs`
  (перевод чужого sentinel'а; паника на nil target), `ValidSlug`
  (`^[a-z0-9]+(-[a-z0-9]+)*$`, до `MaxSlugLen` = 64) и одиннадцать
  конструкторов-удобств. Наружу уходит только слаг: причина — для лога.
- `errs/httperr` — единственная точка «ошибка → HTTP». `Config` (`RequestID`,
  `Translate`, `Logger`, `InternalSlug`), `New` с паникой на негодном
  `InternalSlug`, `Responder.Write`: `Translate` первым и один раз, статус по
  `Kind`, тело `{"slug","request_id"}` (пустой `request_id` опускается),
  `Content-Type: application/json; charset=utf-8` и `Cache-Control: no-store`.
  Не-`SlugError` и `SlugError` с непроверенным слагом — 500 и `InternalSlug`,
  без текста чужой ошибки. `Retry-After` — только из структурного контракта
  `RetryAfter() (time.Duration, bool)`, с округлением вверх. Уровни лога:
  5xx — Error, 401/403/429 — Warn, прочие 4xx — Debug; отменённый `ctx` — без
  лога. Ответ не пишется дважды: обёртка с `Written() bool` (gin) или
  `Status() int` (chi) останавливает запись. `StatusOf` (503 у `Unavailable`,
  не 502; 504 у `Timeout`) и `StaticBody` для `http.TimeoutHandler`.
- `errs/errstest` — guard-тесты потребителя: `NoDirectHTTPErrors` (обход
  дерева по токенам, а не по тексту: `http.Error` и рукописные тела
  `{"slug":…}`/`{"error":…}` в строковых литералах; комментарии, `_test.go`,
  `testdata`, `vendor` и пути из `allow` пропускаются), `CheckSlugRegistry`
  (годность и уникальность), `KindStatusTable` (у каждого `Kind` есть статус).
  Внутри стражи пишут в интерфейс `reporter`, поэтому их собственные тесты
  проверяют находки на фикстурах, а не падают вместе с ними.
- `reqid` — идентификатор запроса: `Middleware` (принять чужой
  `X-Request-Id` только в форме `[A-Za-z0-9._-]{1,128}`, иначе выдать свой из
  16 байт `crypto/rand` в base64url; эхо в ответе), `From`, `With`. Роутера не
  знает. Fuzz на свойство «наружу уходит только безопасная форма».
- `config` — загрузка окружения: `Loader` копит проблемы, `Err()` отдаёт их
  одним `errors.Join` строками «KEY: причина». Читатели `Required`,
  `Optional`, `Secret`, `Duration`, `Int`, `Port`, `Bool`, `Enum`, `Ratio`,
  `CSV`, `URL`, плюс `Fail` для контекстных проверок вызывающего. Ошибки
  программиста (негодное умолчание, пустой список `Enum`, `minLen <= 0`) —
  паника с текстом «X must …». Тип `Secret` редактируется в `fmt` (`%v`,
  `%+v`, `%#v`, `%q`), `slog` и `json`; значение достаёт только `Reveal`.
- `retry` — повторы с полным джиттером: `Backoff.Delay` (перенесён из
  `mail`, с защитой от переполнения сдвига), `Policy`/`Retrier` с
  panic-валидацией и подменяемым `Sleeper`, `Do`, обёртки `Permanent` и
  `Throttled`, `IsPermanent`/`RetryAfterOf`, закрытый `Class` с `AllClasses`
  и `Classify` по структурному контракту, `ParseRetryAfter` (секунды или
  HTTP-date), `ErrAttemptsExhausted` и `ErrRetryAfterTooLong`.
- `secrets` — AES-256-GCM с версией блоба и ротацией: `KeyID`, `Keyring`
  (`NewKeyring` с копированием ключей, `ParseKeyring` строки
  `"1:<base64>,2:<base64>"` — активный ключ наибольший, база64 с
  выравниванием и без), `GenerateKey`, `DeriveKey` (HKDF-SHA256, salt=nil,
  info=purpose, секрет от `MinSecretLen`), `Cipher` с `Seal`/`Open`/
  `KeyIDOf`/`Reseal(blob, aad) (out, changed, err)`, ошибки `ErrMalformed` и
  `ErrUnknownKey`. Формат блоба: версия(1) | keyID(2, BigEndian) | nonce(12) |
  ciphertext+tag; заголовок открыт и входит в AAD. Сам формат — контракт
  совместимости наравне со схемой адаптера: его смена возможна только с
  миграцией через `Reseal` и считается ломающим изменением.
- `ratelimit` — token bucket на ключ без фоновой горутины: `Config`
  (`Limit`, `Window`, `Burst` = 0 → `Limit`, `IdleTTL` > `Window`, `MaxKeys`) с
  panic-валидацией, порт `Gate`, `Decision{Allowed, Remaining, RetryAfter}`,
  `Allow`, `Sweep(ctx) (int, error)` под сигнатуру задачи планировщика,
  `Stats{Keys, Overflows}`, `SetClock`. Пустой ключ — отказ и `ErrEmptyKey`;
  на переполнении `MaxKeys` сначала inline-sweep простаивающих, потом отказ
  новым ключам со счётчиком.
- `ratelimit/ratelimithttp` — `Middleware(Gate, KeyFunc, Deny)` (ставит
  `Retry-After` с округлением вверх, тело пишет `httperr` потребителя),
  `ClientIP(r, trusted []netip.Prefix)` — `X-Forwarded-For` справа налево
  только от доверенного соседа, `ByIP(trusted, v6PrefixBits)` со сворачиванием
  IPv6 в префикс.

### Security
- `retry`: переполнение сдвига оставляет потолок `Max`, а не ноль — иначе
  джиттер пропал бы ровно на дальних попытках; `Retry-After` дольше
  `Policy.MaxRetryAfter` возвращает управление планировщику вместо сна в
  задаче; мусорный заголовок — «подсказки нет», а не ноль.
- `secrets`: заголовок блоба (версия, keyID) входит в аутентифицируемые
  данные — понижение версии формата и подмена номера ключа не проходят
  проверку тега; `ErrUnknownKey` отделён от `ErrMalformed` как операционный
  сигнал «ключ убрали до перешифровки». Разбор кольца не пишет в ошибку ни
  одного байта ключа, а `Keyring` и `Cipher` редактируются во всех формах
  печати (`fmt` любым глаголом через `Format`, `slog`, `json`): без этого
  `%v` достал бы ключи рефлексией из неэкспортируемых полей.
- `ratelimit`: пустой ключ и ошибка `Gate` — отказ, а не пропуск;
  `X-Forwarded-For` читается только от доверенного прокси, иначе лимит
  обходится одним заголовком; IPv6 агрегируется по префиксу; ключ (IP — ПДн)
  не логируется и в ответ не попадает.
- Значение переменной окружения никогда не попадает в текст ошибки `config`:
  список проблем идёт в лог выката, а под ключом лежит DSN с паролем.
  Длина секрета считается в символах, умолчания у секрета нет, пустая
  переменная равна незаданной.
- `httperr` наружу отдаёт только слаг и `request_id`; текст чужой ошибки не
  уходит клиенту ни при каких условиях, слаг собранной вручную `SlugError`
  проверяется `ValidSlug` перед записью.
- `reqid` принимает чужой заголовок только в безопасной форме: CR и LF из
  него разрезали бы строку лога и заголовки ответа.
- Toolchain go1.26.6: govulncheck проверяет ту stdlib, которой собран модуль;
  пакеты ходят в `net/http`, `net/url` и `crypto/rand`.
