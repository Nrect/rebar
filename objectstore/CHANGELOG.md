# Changelog — objectstore

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- Сценарий `RunStoreSuite` «ModifiedAt — в UTC у Put и у List»: контракт
  `Object.ModifiedAt` из `ports.go` набор до сих пор не сторожил. Точность
  момента у реализаций своя и контрактом не является — `fs` отдаёт наносекунды
  файловой системы, `s3` секунды заголовка `Date` у `Put` и миллисекунды
  `LastModified` у `List`, — поэтому сценарий сравнивает зону, а не момент.
  Усечения до микросекунд, как у двойников с pg-адаптером, здесь нет
  намеренно: `timestamptz` в модуле нет, и усекающий двойник разошёлся бы с
  `fs` в том же бинаре. Двойник в наборе `fs` идёт на часах в чужой зоне: на
  часах в UTC сценарий у него зелен и без приведения. Ломающее для своей
  реализации `Store`, которая отдаёт местное время.

### Changed
- **Класс ошибки у sentinel ([ADR-0007](../docs/adr/0007-error-kind.md)).**
  Потребителю больше не нужна таблица перевода ошибок `objectstore`:
  `errs.KindOf` и `httperr` находят класс на самой sentinel. Страж
  `errstest.EveryErrorHasKind` стоит в корне модуля (`sentinels_test.go`);
  двойники `objectstoretest` из него исключены — их ошибка только причина,
  класс несёт обёртка ядра, и это держит
  `TestPortFailuresReachCallerAsUnavailable` на путях `Uploader.Upload` и
  `Collector.Run`.

  | Sentinel | Класс | Почему |
  |---|---|---|
  | `ErrTooLarge` | 413 `payload-too-large` | тело присылает клиент, и у превышения размера свой класс |
  | `ErrEmptyBody`, `ErrUnsupportedType`, `ErrSVGRejected` | 400 `incorrect-input` | тело загрузки и его содержимое присылает клиент |
  | `ErrBadKey`, `ErrSizeUnknown` | 400 `incorrect-input` | ключ и размер приходят с запросом (ADR-0007, «Спорные назначения») |
  | `ErrUnavailable` | 503 `unavailable` | сбой хранилища, повтор осмыслен |
  | `ErrBadMethod`, `ErrBadTTL` | нет, `//errs:nokind` | метод и срок ссылки задаёт код потребителя: негодные — дефект, то есть 500 |
  | `ErrNotFound` | нет, `//errs:nokind` | в модуле её отдаёт только `s3` на ответ 404, а у `Put`, `Delete` и `List` это нет бакета — дефект конфигурации, то есть 500. Расходится с правилом примера-монолита (404 `file-not-found`) |
  | `ErrCursorStuck` | нет, `//errs:nokind` | курсор тот же, и повтор не поможет; путь фоновый (ADR-0007, «Спорные назначения») |

- **Ломающее для кода, который сравнивал тексты sentinel, присваивал их или
  звал `SetClock(nil)`.** Замена:

  | Было | Стало |
  |---|---|
  | тип sentinel с классом — `error` | `errs.KindError`; `errors.Is` и `==` работают как прежде, но переменная, выведенная из неё (`kind := objectstore.ErrUnavailable`), больше не принимает sentinel без класса — объявлять `var kind error` (так правлен `s3.statusError`) |
  | `object body is …`, `object content type is not accepted`, `svg is never accepted: …`, `object key is …`, `object size is …`, `object is not found` | тот же текст с префиксом `objectstore: ` |
  | `presign method is not one of AllMethods`, `presign ttl is …` | тот же текст с префиксом `objectstore: ` |
  | `object store operation could not be completed` | `objectstore: operation could not be completed` |
  | `object store returned a cursor that does not advance` | `objectstore: store returned a cursor that does not advance` |
  | `Collector.SetClock(nil)` и `s3.Store.SetClock(nil)` принимались и падали разыменованием при первом обращении к часам | паника `objectstore.Collector.SetClock: now must not be nil` и `objectstore/s3.Store.SetClock: now must not be nil` |

  Префикс не косметика: `KindError` равны по классу и тексту, и без него
  `objectstore.ErrUnavailable` совпала бы через `errors.Is` с
  `ErrUnavailable` другого модуля того же текста. Модуль требует
  `github.com/nrect/rebar/kit v0.2.0`.
- **API двойников меняется ломающе: настройка `objectstoretest.MemStore` и
  `objectstoretest.MemOwned` — методы, а не публичные поля.** Двойники читали
  поля под своим мьютексом, а тест писал их мимо него. Пока поле ставится до
  первого вызова, гонки нет; но тест потребителя, у которого загрузку или
  сборщик зовёт другая горутина, пишет поле, пока двойник его читает, — и
  `-race` краснеет у потребителя
  ([CONVENTIONS §3](../CONVENTIONS.md#3-двойники)). Замена:

  | Было | Стало |
  |---|---|
  | `store.Err = err` | `store.SetErr(err)` |
  | `store.Now = now` | `store.SetClock(now)` |
  | `owned.Err = err` | `owned.SetErr(err)` |

  Ошибки снимаются `nil`. `SetClock` — метод, как у ядра
  (`Collector.SetClock`, `s3.Store.SetClock`); часы зовутся под замком
  двойника, изнутри `Put`. `nil` вместо часов у двойника по-прежнему поломка
  стенда, а не паника, как у ядра: `Put` ответит `ErrDoubleBroken`.
- Контракт `Store.Put`: `ModifiedAt` в ответе — отметка ответа хранилища, а
  не момент объекта. У `s3` это заголовок `Date`, а `List` отдаёт
  `LastModified` самого объекта: величины разные по смыслу, а не по точности,
  и сравнивать их нельзя — кому нужен момент объекта, читает `List`. Равными
  их сделал бы только лишний `HEAD` после каждой загрузки.

### Fixed
- Интеграционный тест `s3` берёт образ MinIO с `quay.io`, а не с Docker Hub:
  тот отвечает «pull access denied … repository does not exist» без
  `docker login`, и `make chip-check MODULE=objectstore` краснел у всех.
  Digest прежний, побайтно тот же образ — переехал только источник.
- Интеграционный тест `s3` ждёт кворума записи MinIO (`/minio/health/cluster`),
  а не жизни процесса (`/minio/health/live`): `live` отвечает 200 раньше, чем
  поднят слой объектов, и создание бакета в это окно получало 503
  `XMinioServerNotInitialized` — `TestMain` изредка падал, а с ним `chip-check`
  и сбор покрытия gremlins. `/minio/health/ready` у этого релиза не лечит: на
  40 стартах с частым опросом бакет получил 503 после `live` 10 раз, после
  `ready` 14 раз, после `cluster` ни разу. Повтор на 503 не взят: он спрятал бы
  и настоящую недоступность.

## [0.1.0] — 2026-09-10

### Added
- Каркас пакета по [ADR-0006](../docs/adr/0006-objectstore.md): порт `Store`
  (`Put`/`Delete`/`List`/`Presign`/`PublicURL`), порт `Owned`, закрытые наборы
  `Method`, `ContentType` и `CollectMode` со списками `All*` и guard-тестами,
  `CheckKey` как общая проверка ключа для ядра и адаптеров, страж импортов с
  белым списком по каталогам и корпусом `testdata/badcore` из двух классов
  нарушения.
- `Uploader` с пятью инвариантами приёма: потолок размера до чтения тела, тип
  по содержимому, безусловный отказ SVG, ключ `<Prefix>/<uuid>.<ext>` нашей
  постройки, имя файла пользователя мимо ключа. `UploaderConfig` с
  panic-валидацией.
- `Collector` с двумя предохранителями (`MinAge` и режим `CollectMode`) и
  сигнатурой `Run(ctx) (int, error)` — это `scheduler.Job.Run`, импорта
  планировщика нет. Сбой `IsOwned` останавливает прогон; курсор, который не
  двигается, обрывает обход `ErrCursorStuck`.
- Адаптер `s3`: SigV4 на stdlib (подпись заголовком и query-строкой),
  обязательный path-style, `ListObjectsV2` с курсором, подписанные ссылки.
  Проверен официальным AWS SigV4 test suite и живым MinIO в testcontainers.
- Адаптер `fs` для разработки: запись через временный файл, локальные ссылки,
  отказ на ключах, выходящих за корень.
- Адаптер `imgproxy`: `SignedURL(key, Options)`, HMAC-SHA256 на stdlib; вектор
  сверен с эталонной реализацией самого imgproxy.
- Двойники `objectstoretest.MemStore` и `MemOwned` и контрактный набор
  `RunStoreSuite(t, factory)` на голом `testing`; набор гоняется по двойнику,
  по `fs` (в одном бинаре) и по MinIO.

### Changed
- `golang.org/x/crypto` поднят до v0.56.0: `testcontainers-go` тянет его
  косвенно и приводил v0.54.0, а общая версия во всех модулях тулкита —
  v0.56.0 (VERSIONING, «Единая версия Go и общих зависимостей»). Правка
  внутри `objectstore/go.mod`, чужие модули не тронуты.

### Security
- Ключ объекта и имя файла пользователя не попадают в логи, тексты ошибок и
  тексты паник: ключ бывает выведен из персональных данных. Из ответа S3
  берётся только `Code` (в `Message` и `Key` провайдер кладёт ключ целиком),
  ошибка транспорта не заворачивается (в `*url.Error` лежит адрес запроса),
  ошибка `fs` не несёт пути (в `*fs.PathError` он полный).
- `fs` отвергает ключ, уходящий за корень **через символическую ссылку**:
  `filepath.Clean` чистит путь лексически и ссылки не видит, поэтому каталог
  внутри корня, ведущий наружу, пропускал бы запись мимо проверки. Найдено
  тестом `TestFS_DoesNotFollowSymlinkOutOfRoot` при написании пакета.
- PDF остаётся в наборе, но условие названо вслух: он умеет нести JavaScript,
  и браузер рисует его встроенным просмотрщиком. Подписанная ссылка ведёт на
  origin бакета и безопасна; свой CDN на своём домене переносит файл в наш
  origin, и тогда нужен `Content-Disposition: attachment`. Пункт 11 в
  «Безопасность» `doc.go` и предупреждение у самого поля `s3.Config.PublicBase`,
  где этот выбор и делается.
- Отказ SVG держится ДВУМЯ рубежами, и второй теперь под тестом. Поиск `<svg`
  смотрит первые `SniffLen` байт и маркер за окном не ловит; такой файл
  отвергает белый список типов (пролог даёт `text/xml`, голый комментарий —
  `text/html`, ни того ни другого в наборе нет). Связка держится на том, что
  набор закрыт кодом, а не настройкой: `TestUploader_RejectsSVGHiddenBeyondTheSniffWindow`
  краснеет, если в набор добавить `text/xml` (проверено подстановкой).
- Режим сборщика — закрытый набор `CollectMode`, а не булев `DryRun`: нулевое
  значение `bool` — это «удалять», и забытое поле снимало бы предохранитель
  молча.
