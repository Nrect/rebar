# idem — ответ на повтор запроса по ключу `Idempotency-Key`

Клиент шлёт `POST` или `PATCH` с заголовком `Idempotency-Key`. Первый запрос
исполняет операцию и в той же транзакции записывает её ответ; повтор с тем же
ключом получает записанный ответ байт в байт с `Idempotent-Replayed: true`.
Тот же ключ с другим запросом — 409, параллельный повтор — 409 с
`Retry-After: 1`. Решения и доводы — [ADR-0012](../docs/adr/0012-inbox-idempotency.md),
решения 9–17.

Состояние: ядро, двойник с контрактным набором, `idemhttp` и хранилище Postgres
`idempg`. Метрики `idemotel` — следующий шаг; до него наблюдатель —
`idem.LogObserver`.

## Когда брать

- ручка создаёт или меняет что-то локально — заказ, заявку, профиль, — и клиент
  повторяет запрос после таймаута или обрыва;
- эффект целиком в базе проекта, а внешнее уходит сообщением `outbox` в той же
  транзакции.

## Когда не брать

- **ручка зовёт чужой API** — вызов в транзакции `idem` не откатывается;
  внешний эффект — в `outbox` изнутри op, либо ручка идемпотентна доменом;
- **оплата** — ключ `payment` живёт вечно и дозавершает попытку у провайдера.
  Ручка оплаты берёт ключ тем же `idemhttp.Key(r)` и отдаёт `key.String()` в
  `payment.StartRequest.IdempotencyKey`, но в `idem` не заворачивается;
- **ответ с одноразовым секретом** (ключ API, токен восстановления) — тело
  хранится как есть до конца срока;
- **`PUT` и `DELETE`** — идемпотентны сами;
- **вызов сервис-к-сервису без сессии** — области нет.

## Как подключить

Пример целиком — [example_test.go](example_test.go), он собирается и
проверяется `go test`. Порядок в ручке:

1. Прочитать тело с потолком и разобрать его **до** `Do`: запрос, не прошедший
   проверку, ключ не расходует.
2. Область — принципал сессии: `idem.Scope{Realm: p.Realm.String(), Subject:
   p.SubjectID.String()}` из `authhttp.PrincipalFrom`. Не поле тела и не
   заголовок.
3. `idemhttp.NewRequest(r, scope, "orders.create", body)` — ключ, метод, путь
   и строка запроса как пришли.
4. `store.Do(ctx, req, op)`: всё, что меняет состояние, — внутри op; op
   возвращает `idem.Response` (`idemhttp.JSON`) или ошибку.
5. Ошибку — своим ответчиком (`httperr.Responder.Write`), результат —
   `idemhttp.Write(w, res)`.

Сборка на старте: `Config` с набором операций, сроком и потолком ответа;
конструкторы паникуют на негодном. Уборка — `idem.NewPurger(store, cfg).Run`
задачей планировщика, например раз в час.

```go
cfg := idem.Config{
	Operations:       []idem.Operation{"orders.create", "profile.update"}, // закрытый набор: метка метрики
	Retention:        48 * time.Hour,                                      // не меньше суток и окна повторов клиента
	MaxResponseBytes: 64 << 10,                                            // тело и заголовки; больше — откат
}
```

## Хранилище Postgres

Схема — миграции goose, их отдаёт `idempg.Migrations()` (`fs.FS`). Накатывает
раннер проекта со своей таблицей версий `idem_schema_version`: без
`WithTableName` goose пишет в общую `goose_db_version`, и номера блоков тулкита
в ней столкнутся. Модуль goose не импортирует и схему не применяет.

```go
func migrateIdem(ctx context.Context, db *sql.DB) error {
	p, err := goose.NewProvider(goose.DialectPostgres, db, idempg.Migrations(),
		goose.WithTableName("idem_schema_version"))
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}
```

Сборка на старте — тот же `Config`, что у двойника, и наблюдатель.
`CheckSchema` зовут старт и `/readyz`: он сверяет колонки, ограничения и
индексы и называет каждое расхождение. Роли приложения достаточно `SELECT`,
`INSERT` и `DELETE` на `idem_records`.

```go
store := idempg.New(pool, cfg, idem.LogObserver(logger))
if err := store.CheckSchema(ctx); err != nil {
	return err // миграции не накатаны или схема расходится с ними
}
purge := idem.NewPurger(store, cfg) // purge.Run — задача планировщика
```

В ручке op получает транзакцию `Do`: эффект пишется в неё, и ответ ложится
вместе с ним.

```go
res, err := store.Do(r.Context(), req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
	id, err := orders.WithTx(tx).Create(ctx, in)
	if err != nil {
		return idem.Response{}, err // откат, записи нет: повтор исполнит op заново
	}
	resp, err := idemhttp.JSON(http.StatusCreated, map[string]string{"order": id})
	resp.Location = "/orders/" + id
	return resp, err
})
```

Транзакция уже открыта ручкой — `store.WithTx(tx).Do(ctx, req, op)`. **Ошибку
`Do` в `WithTx` не игнорировать:** адаптер прерывает транзакцию, и `COMMIT`
вернёт `commit unexpectedly resulted in rollback`, а в логе базы и трассировке
будет запрос с текстом `idempg: транзакция прервана после отказа`. Иначе эффект
закоммитился бы без записи, и повтор исполнил бы его второй раз.

## Что нельзя ломать

- **Область — только из сессии.** Ключ уникален в паре с принципалом: область
  из запроса отдаёт чужой ответ угадавшему ключ.
- **До `Do` — только чтение.** Записанное до `Do` закоммитится и на повторе, где
  op не зовётся.
- **Отказ, который повтор обязан получить тем же, — ответ 4xx из op**, а не
  ошибка: ошибка откатывает транзакцию, и повтор исполнит op заново.
- **5xx не записывается никогда:** `ErrNotRecordable` и откат. Сбой — это
  ошибка op.
- **В ответе только `Content-Type` и `Location`.** Куку и прочие заголовки
  ставить вне `idem`; тело без `Content-Type` не записывается.
- **Ключ, область и тело не логируются.** `LogObserver` пишет только операцию
  и исход.

## Ошибки

| Ошибка | Ответ | Когда |
|---|---|---|
| `ErrKeyMissing`, `ErrKeyInvalid` | 400 | нет заголовка или он не по форме |
| `ErrKeyReused` | 409 | тот же ключ, другой запрос или другая операция |
| `ErrInFlight` | 409 и `Retry-After: 1` | тот же ключ сейчас в транзакции |
| `ErrUnavailable` | 503 | хранилище не ответило |
| `ErrNotRecordable`, `ErrResponseTooLarge`, `ErrInvalidScope`, `ErrInvalidRequest` | 500 | дефект ручки: ответ, его размер, область, операция или метод |
| ошибка op | её класс | записи нет, повтор исполнит op заново |

Слаги у двух 409 потребитель вправе развести в `Translate`: по занятому ключу
клиент ждёт и повторяет, по переиспользованному — чинит клиент.

## Тесты

`idemtest.NewMemStore(cfg, obs)` ведёт себя как адаптер: исключение по ключу
без общего замка (параллельный вызов получает `in_flight`, а не ждёт), срок,
моменты как в `timestamptz`, отмена и `SetErr` в `ErrUnavailable`. Код теста
отличается от прода конструктором хранилища и формой op — без `pgx.Tx`.
Ручки двойника — сеттеры под замком: их можно дёргать, пока ручка обслуживает
запрос.

## Наблюдаемость

`Observer` обязателен у хранилища: `idemotel` (следующий шаг) или
`idem.LogObserver`. Метрика `idem_requests{operation,outcome}` и алерты —
ADR-0012, решение 16: «ответ не записывается» (`not_recordable|too_large`,
порог 1), «ключи переиспользуют» (`reused`), «уборка умерла»
(`cron_*{job="idem_purge"}`).

Инварианты и «Чего нет» — [doc.go](doc.go); изменения — [CHANGELOG.md](CHANGELOG.md).
