# Changelog — auth

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Changed
- **API двойников меняется ломающе: настройка двойников `authtest` — методы,
  а не публичные поля.** Двойники читали поля под своим мьютексом, а тест
  писал их мимо него. Пока поле ставится до первого вызова, гонки нет; но тест
  потребителя через живой HTTP-сервер пишет поле, пока ручка сервера его
  читает, — и `-race` краснеет у потребителя
  ([CONVENTIONS §3](../CONVENTIONS.md#3-двойники)). Замена:

  | Было | Стало |
  |---|---|
  | `m.Err = err` у `MemIdentities`, `MemSessions`, `MemTokens`, `MemAttempts`, `RecordingNotifier`, `RecordingAuditor` | `m.SetErr(err)` |
  | `sessions.TouchErr = err` | `sessions.SetTouchErr(err)` |
  | `attempts.RecordErr = err` | `attempts.SetRecordErr(err)` |
  | `ids.NewID = gen` | `ids.SetIDs(gen)` |
  | `strength.Default = score` | `strength.SetDefault(score)` |
  | `strength.ByPassword[pw] = score` | `strength.Set(pw, score)` |
  | `m.Calls.CallCount(name)` | `m.CallCount(name)` |

  Ошибки и генератор снимаются `nil`. Счётчик `Calls` у `MemSessions`,
  `MemTokens` и `MemAttempts` больше не поле: он встроен скрыто, `CallCount`,
  `Snapshot` и `Reset` остались на двойнике. Генератор идентификаторов зовётся
  под замком двойника, изнутри `Create` и `Put`, и трогать двойник не вправе.
- **Класс ошибки у sentinel ([ADR-0007](../docs/adr/0007-error-kind.md)).**
  Потребителю больше не нужна таблица перевода ошибок `auth`: `errs.KindOf` и
  `httperr` находят класс на самой sentinel. Страж
  `errstest.EveryErrorHasKind` стоит в корне модуля (`sentinels_test.go`) и
  обходит все подпакеты; двойники `authtest` из него исключены — их ошибки
  только причины, класс несёт обёртка ядра `session`, и это держит
  `session.TestPortFailuresReachCallerAsUnavailable`.

  | Sentinel | Класс | Почему |
  |---|---|---|
  | `ErrUnavailable` | 503 `unavailable` | сбой источника личностей, повтор осмыслен |
  | `ErrLoginTaken` | 409 `conflict` | единственный выход наружу — `session.ConfirmEmailChange`: ссылку открыл владелец нового адреса (ADR-0007, «Спорные назначения») |
  | `session.ErrInvalidCredentials`, `session.ErrNoSession` | 401 `unauthenticated` | вход не состоялся либо живой сессии нет |
  | `session.ErrNotVerified`, `authhttp.ErrCSRF` | 403 `forbidden` | личность известна, но операция ей не положена: адрес не подтверждён, запрос отправил не сам клиент |
  | `session.ErrTooManyAttempts`, `password.ErrBusy` | 429 `too-many-requests` | исчерпан счётчик попыток или занят потолок хеширований; `Retry-After` класс не добавляет — `httperr` берёт его только из `RetryAfter()` |
  | `session.ErrTokenInvalid`, `loginid.ErrInvalid`, `password.ErrTooShort`, `ErrTooLong`, `ErrTooWeak` | 400 `incorrect-input` | токен, логин и пароль присылает клиент; три причины `Policy` делят один класс, а различать ли их слагом — словарь потребителя |
  | `ErrInvalidRealm`, `token.ErrSecretTooShort` | нет, `//errs:nokind` | реалм и секрет задаёт конфигурация сборки: негодные — дефект, то есть 500 |
  | `ErrIdentityNotFound` | нет, `//errs:nokind` | сигнал порта `Identities`: `session` сворачивает её на всех путях, а готовый 404 на входе или сбросе — перебор адресов (ADR-0007, «Спорные назначения») |
  | `password.ErrHashInvalid` | нет, `//errs:nokind` | строку хэша пишет хранилище потребителя: битая колонка — инцидент, то есть 500 |

- **Ломающее для кода, который сравнивал тексты sentinel или присваивал их.**
  Замена:

  | Было | Стало |
  |---|---|
  | тип sentinel с классом — `error` | `errs.KindError`; `errors.Is` и `==` работают как прежде |
  | `realm is invalid`, `identity not found`, `login is already taken`, `identity store is unavailable` | тот же текст с префиксом `auth: ` |
  | `invalid credentials`, `too many attempts`, `identity is not verified`, `no live session`, `one-time token is invalid` | тот же текст с префиксом `session: ` |
  | `password hashing is at capacity`, `password hash is malformed or out of bounds`, `password is shorter than the minimum length`, `password exceeds the maximum length`, `password is too easy to guess` | тот же текст с префиксом `password: ` |
  | `login is invalid` | `loginid: login is invalid` |
  | `realm secret is shorter than the minimum length` | `token: realm secret is shorter than the minimum length` |
  | паника `token.MustSecret: realm secret is shorter …` | `token.MustSecret: token: realm secret is shorter …` |
  | `csrf token mismatch` | `authhttp: csrf token mismatch` |

  Обёртки вокруг `auth.ErrUnavailable` (`session`, `authpg`,
  `authtest.ErrSessionExists`) начинаются с нового текста. Префикс не
  косметика: `KindError` равны по классу и тексту, и без него sentinel разных
  модулей с одним классом совпадали бы через `errors.Is`. Модуль требует
  `github.com/nrect/rebar/kit v0.2.0` — в корне и в каталогах `session`,
  `password`, `loginid`, `authhttp`.

### Fixed
- **Комментарии объявляли инвариант безопасности без исключения, которое в
  коде есть.** `errors.go` («`ErrLoginTaken` наружу не выходит») и пункт 2
  «Безопасности» в `doc.go` («ЗАНЯТЫЙ ЛОГИН НЕ ВИДЕН СНАРУЖИ») обещали
  абсолют, а `session.ConfirmEmailChange` отдаёт `auth.ErrLoginTaken`
  осознанно: ссылку открыл владелец нового адреса, и «занят» он узнал бы и
  восстановлением пароля. Потребитель, читающий список «Безопасность», верил
  бы в неправду. Оба комментария называют единственный законный выход и
  довод; поведение не менялось.

## [0.1.0] — 2026-09-10

### Added
- Корень модуля: `Realm` с формой `[a-z0-9_]{1,32}` как контрактом со схемой
  адаптера, `Principal` (хэш сессии, не токен), `Identity`, порт `Identities`
  (таблица пользователей — у потребителя) и сентинелы `ErrInvalidRealm`,
  `ErrIdentityNotFound`, `ErrLoginTaken`, `ErrUnavailable`.
- `auth/password`: argon2id в формате PHC (`$argon2id$v=19$m=..,t=..,p=..$..$..`)
  с потолками параметров **при проверке** — враждебная строка в колонке не
  заставит процесс выделить гигабайт, а строка длиннее `MaxEncodedLen` не
  дойдёт даже до деления по `$`; `Hasher` с общим на процесс потолком
  одновременности (2..4 слота) и отдельной ошибкой `ErrBusy`, которая не
  оборачивает контекст; `Equalize` — холостое хеширование ради выравнивания
  времени ответа на несуществующий логин; гейджи `InFlight`/`Slots`;
  `Policy` — длина плюс порог силы по префиксу в 64 байта, без составных
  правил (NIST 800-63B), с обязательным портом `StrengthChecker`.
- `auth/token`: `Generate` (256 бит из crypto/rand, base64 URL-safe),
  `Hash` (HMAC-SHA256 под секретом реалма, hex), тип `Secret` с проверкой
  длины и редакцией во всех формах печати (`fmt.Formatter`, `slog.LogValuer`,
  `json.Marshaler`), закрытый набор `Purpose` с `AllPurposes`.
- `auth/pwzxcvbn`: адаптер порта `StrengthChecker` на
  `github.com/trustelem/zxcvbn` — единственная внешняя зависимость каталога.
- `auth/authtest`: `FastHasher` (настоящий argon2id на полах OWASP), двойник
  `Strength` с заданным ответом и `MemIdentities` — двойник порта `Identities`
  с настоящей уникальностью логина, хранением пришедших параметром времён и
  отличимой от доменных инъекцией отказа (`ErrInjected`).
- Страж импортов с белым списком по каталогам, корпусом в `testdata` и
  тестом на **срабатывание**; тест «в портах нет чужих типов».
- Тесты: `FuzzDecode` по разбору PHC-строки (корпус перенесён из кода-предка),
  `TestHasher_NeverExceedsCap` (24 горутины на два слота, по одному прогону на
  каждую из трёх дверей в семафор), `TestHasher_BusyIsIdenticalForVerifyAndEqualize`
  (паритет ответа при перегрузке), границы `Policy`, редакция секрета
  поимённо по глаголам печати, guard закрытого набора `Purpose`.

- `auth/loginid`: `Normalize`, `MaxLen`, `ErrInvalid` — единственная точка
  нормализации логина (обрезка, NFKC, нижний регистр). Счётчик блокировок
  ведётся по логину, поэтому разные написания одного адреса обязаны давать один
  ключ, иначе блокировка обходится клавишей Shift. Порт `Identities` получает
  логин уже нормализованным; уникальный индекс потребитель строит на
  сохранённой колонке. Отдельным каталогом ради `golang.org/x/text`: корень
  импортируют все, включая слой хранения потребителя, и таблицы Unicode в его
  графе зависимостей — последнее, чего он ждёт.
- `auth/session`: `Service` одного реалма — `Register` (семантика «принято»:
  занятый адрес отвечает так же, а письмо уходит владельцу), `SignIn`
  (`ErrInvalidCredentials | ErrTooManyAttempts | ErrNotVerified`, результат, а
  не голый токен — шов для второго фактора), `Resolve` со скользящим
  продлением не чаще `RenewEvery` и немедленным отказом отключённому субъекту,
  `SignOut`/`SignOutAll`, `ChangePassword` с отзывом ВСЕХ сессий и ВСЕХ токенов
  сброса, `Request/Confirm` для подтверждения адреса, сброса пароля и смены
  логина, `Sweep(ctx) (int, error)` в подписи `scheduler.Job.Run`, `SetClock`.
  Порты `Sessions`, `Attempts` (ключ — нормализованный логин, в том числе
  несуществующий), `Tokens` (реализует ПОТРЕБИТЕЛЬ), `Notifier`, `Auditor`
  (nil допустим); закрытые наборы `NotificationKind` и `EventKind` с `All*`.
  `Config` с шестью инвариантами ADR-0003 и потолками `ResetTTL <= 1h`,
  `VerifyTTL <= 72h`; `Secret` типом `token.Secret`, поэтому паника
  `token.Hash` на ненастроенном секрете из рабочего кода недостижима.
- `auth/session/doc.go`: таблица «метрика — тип — что» с закрытыми наборами
  меток и порогами алертов. Декоратора в v0.1 нет осознанно: счётчик снимается
  там же, где живёт `httperr`, и второй источник тех же цифр разошёлся бы с
  первым.
- `auth/authpg`: `Store` (`session.Sessions` + `session.Attempts`) и
  `Store.Tokens()` — половина порта `Tokens` на `postgres.Querier`: `Insert`,
  `ConsumeRow`, `RevokeOfSubject`, `PurgeExpired`. `New(pool)`, `WithTx(tx)`,
  `schema.sql` с goose-маркерами и тремя таблицами ADR-0003 (`auth_sessions`,
  `auth_tokens`, `auth_login_attempts`), `Schema` и `CheckSchema`, который
  сверяет и ничего не меняет. Ошибки через общий `postgres.Sanitize`.
- `auth/authhttp`: `CookieConfig` с инвариантами (`SameSite` обязателен явно,
  `SameSite=None` требует `Secure`, префикс `__Host-` требует `Secure`,
  `Path="/"` и пустого `Domain`), `Middleware`, `PrincipalFrom`,
  `WithPrincipal`, `SetSession` (сессионная кука `HttpOnly`, CSRF-кука — нет),
  `ClearSession`. CSRF double-submit через `subtle.ConstantTimeCompare` на
  небезопасных методах; 401 и 403 пишет `Deny` потребителя.
- `auth/authtest`: двойники портов второй половины — `MemSessions`,
  `MemAttempts`, `MemTokens` (связан с `MemIdentities` и честно применяет
  эффект назначения токена), `RecordingNotifier`, `RecordingAuditor`, `Clock`,
  счётчик `Calls`. Контрактные наборы `RunSessionsSuite` и `RunAttemptsSuite`
  на голом `testing` — гоняются и по двойнику, и по адаптеру в одном бинаре.
- `Hasher.NeedsRehash` — назначить ли пересчёт хэша при следующем удачном
  входе: параметры ниже нынешних или неразбираемая строка. Хэш с параметрами
  ВЫШЕ нынешних не трогается — пересчёт ослабил бы его.

### Testing
- Названные тесты контрактов: `TestStore_WithTx_IsAtomic` (эффект потребителя
  и строка токена откатываются вместе, чтение из базы ПОСЛЕ отката),
  `TestConsume_IsOnceUnderRace` (восемь транзакций на один токен, выигрывает
  одна) и его же вариант по двойнику, `TestService_SignIn_ParityForUnknownLogin`
  (паритет ответов И обращений к портам, отдельно при `ErrBusy`),
  `TestService_Lockout_CountsUnknownLogins`, `TestSQL_HasRealmInEveryWhere`,
  `TestCSRF_ComparisonIsConstantTime`, `TestService_ChangePassword_RevokesAll`.
- У каждого нового стража есть тест на СРАБАТЫВАНИЕ: `TestSQL_RealmGuardFires`
  и `TestCSRF_ConstantTimeGuardFires` гоняют его по корпусу с заведомым
  нарушением. Страж, который молчит всегда, выглядит ровно как страж, который
  работает.
- gremlins по ядру второй половины (`session`, `token`, `password`, `loginid`;
  адаптеры и двойники исключены): 262 мутанта, 248 убито, 3 выживших,
  11 «не покрытых», efficacy 98,80 %, прогон 1 мин 53 с при бюджете 10 мин.
  Выжившие: два в `password/policy.go` из первой половины (разобраны там же) и
  один эквивалентный — обрезка User-Agent по его же длине это тождество,
  доказано `TestClampUserAgent_IsIdentityAtTheCeiling`. «Не покрытые»: два
  ARITHMETIC_BASE в инициализаторах констант и девять CONDITIONALS_NEGATION на
  строках `case err != nil:` — известный артефакт снятия покрытия на условиях
  `case`. Разбор — в `session/mutants_internal_test.go`. Второй прогон на том
  же коде дал 245 убитых и три TIMED OUT на `password/hasher.go`: известная
  просрочка вместо убийства паникой, разобранная в
  `password/mutants_internal_test.go`. Сумма Killed + Timed out устойчива, а
  список LIVED в обоих прогонах совпал.
- **Починен мигающий тест первой половины**: `password` держал `MaxWait` в
  50 мс при потолке в два слота на процесс, и под `-race` на загруженной машине
  `TestNeedsRehash_CurrentParametersAreLeftAlone` падал «перегрузкой» два раза
  из шести. Дефект был и до второй половины — проверено тем же прогоном на
  `e5f4e4f`. Падающий тест убивает ВСЕХ мутантов своего прогона, поэтому это
  чинилось до разбора выживших (docs/CHIP.md).
- gremlins по ядру первой половины: 148 мутантов, из них 2 выживших и 1 «не покрытый»,
  efficacy 98,67 %, прогон 46 с при бюджете 10 мин. Ещё три при плотной
  загрузке машины получают TIMED OUT вместо KILLED (отчего Killed скачет между
  145 и 148 на одном коде); каждый проверен руками — набор падает за 11 с.
  Все три разобраны в `password/mutants_internal_test.go`: два мутанта границ
  доказуемо эквивалентны (срез строки по её собственной длине — тождество;
  сведение нуля к нулю), «не покрытый» — ARITHMETIC_BASE в инициализаторе
  константы, куда покрытие не приписывается ни при каком наборе тестов.
- `loginid.FuzzNormalize_IsIdempotent` — идемпотентность нормализации логина;
  17,5 млн прогонов чисто. Два падения по дороге лежат регрессионным корпусом
  в `testdata/fuzz`: негодный UTF-8 и буква, у которой приведение регистра
  выводит строку из нормальной формы (поэтому NFKC применяется дважды —
  до приведения регистра и после).

### Security
- Вход отвечает одинаково на «нет логина», «не тот пароль», «битый хэш в
  колонке» и «отключён», а на `password.ErrBusy` — одним
  `ErrTooManyAttempts` для существующего и несуществующего логина. Счётчик
  попыток ведётся по нормализованному логину независимо от его существования и
  НЕ пополняется поверх блокировки: иначе любой желающий запирает чужой адрес
  навсегда. Перегрузка попыткой не считается — всплеск не должен запирать
  законных владельцев.
- Смена пароля отзывает все сессии и все токены сброса, причём ОТЗЫВ ИДЁТ ДО
  записи нового хэша: на сбое записи владелец всего лишь выкинут из своих
  сессий, а обратный порядок оставил бы чужую сессию живой рядом с новым
  паролем.
- Реалм стоит в каждом `WHERE` адаптера (страж по тексту SQL), в базе лежат
  только HMAC, сырой токен не попадает ни в ошибку, ни в событие аудита.
- Сбой хранилища — `auth.ErrUnavailable` (503), а не «неверные данные»:
  непрочитанный счётчик, незаписанная попытка и неудавшееся продление сессии
  закрывают вход, а не пропускают его.
- `loginid.Normalize` отвергает негодный UTF-8, управляющие и невидимые символы
  форматирования. Первое — потому что на испорченной последовательности NFKC
  не идемпотентен (нашёл фаззер): сохранённый логин переставал бы находиться.
  Второе — подделка соседней строки журнала аудита. Третье — логин с символом
  нулевой ширины печатается неотличимо от чужого, а строкой в базе и ключом
  счётчика он другой; NFKC такие символы не убирает.
- Toolchain go1.26.6 и `golang.org/x/crypto` v0.56.0 — версии, единые для всех
  модулей тулкита (VERSIONING, «Единая версия Go и общих зависимостей»).

### Notes
- `github.com/nrect/rebar/postgres` (псевдоверсия `v0.0.0-20260908221018-051fd21c2237`,
  та же, что у `outbox` и `payment`) — вторая из трёх межмодульных
  зависимостей ADR-0005 и только в каталоге `authpg`: `postgres.Sanitize` это
  граница безопасности, и пять её копий в пяти адаптерах — пять шансов
  разойтись. Ядру она по-прежнему запрещена стражем импортов.
- `github.com/jackc/pgx/v5` v5.10.0 и `github.com/testcontainers/testcontainers-go`
  v0.44.0 (второй — только из `_test.go`) — версии те же, что у `outbox` и
  `payment`.
- `golang.org/x/text` v0.41.0 (версия та же, что у `mail`, `otelboot` и
  `postgres`) — нужна ради NFKC, в stdlib нормализации Unicode нет. Заперта в
  каталоге `loginid`: `go list -deps` по корню `auth` её не показывает.
- **Бамп `golang.org/x/text` и версии Go — не рутинное обновление.** Их
  таблицы Unicode задают результат `loginid.Normalize` (NFKC — в `x/text`,
  приведение регистра и категория `Cf` — в stdlib). Смена результата для того
  же сырого входа означает, что сохранённые логины перестают находиться, а
  восстановить их не из чего: в базе лежит нормализованный логин, сырого входа
  нет. Обновлять сознательно, глядя на
  `TestNormalize_GoldenValuesPinTheUnicodeTables`: красный тест значит
  «посмотри, что стало с чужими логинами», а не «поправь ожидание».
- `golang.org/x/crypto` в ядре `password` — отступление от
  «ядро это stdlib»: argon2id в stdlib нет, а собственная реализация в пакете
  аутентификации означала бы свою криптографию. Библиотека одна, каталог один,
  запись в белом списке стража есть.
- `github.com/trustelem/zxcvbn` v1.0.1 поставляется с `go.mod` без секции
  `require`, поэтому его тестовые зависимости (`github.com/test-go/testify`,
  `github.com/google/go-cmp`) попали в наш `go.mod` косвенными. В сборку
  потребителя они не входят: обрезка графа модулей тесты зависимостей не
  собирает. `github.com/dlclark/regexp2` — настоящая транзитивная зависимость
  библиотеки.
