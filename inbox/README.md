# inbox — приём вебхуков: подлинность до базы, дедуп, честный ответ отправителю

Чужая система шлёт событие, пока не увидит 200. `inbox` проверяет подлинность
до похода в базу, узнаёт повтор по смысловому ключу источника, кладёт отметку
в одной транзакции с эффектом обработчика и отвечает так, чтобы событие не
потерялось: 200 — только учтённому и тому, что не обработается никогда.

Решения и причины — [ADR-0012](../docs/adr/0012-inbox-idempotency.md),
инварианты — [doc.go](doc.go), пример подключения, который проверяет
компилятор, — [example_test.go](example_test.go).

**Статус.** Ядро, двойник с контрактными наборами, HTTP-ручка и хранилище
Postgres (`inboxpg`). Метрики (`inboxotel`) — следующий шаг модуля; до него
наблюдатель — `inbox.LogObserver`.

## Когда брать

- отправитель повторяет доставки и ждёт 200: доставка, мессенджер, банк,
  трекер задач;
- **не** для уведомлений о платежах — их принимает `payment.HandleWebhook`, второй
  дедуп там ничего не добавляет (ADR-0012, решение 8);
- **не** для синхронных вебхуков-решений (`Check` у CloudPayments) — это обычная
  ручка;
- **не** для отправки вебхуков наружу — это `outbox`.

## Подключение

**1. Верификатор отправителя пишет проект** — десятки строк по документации
отправителя. Готовых верификаторов провайдеров в модуле нет: синтаксис подписи
у каждого свой. Порядок и ловушки — в контракте `inbox.Verifier`:

- подпись и время — до разбора тела; `inbox.CheckTimestamp` паникует на
  нулевом допуске, сравнение подписи — `hmac.Equal`;
- в `Event.Payload` — только подписанное; у схемы с перечитыванием объекта —
  ответ API, и тип с ключом — из него же;
- ключ `Event.ID` — смысловой: `webhook-id` или `X-GitHub-Delivery`, если
  отправитель обещает один идентификатор на все повторы, иначе
  `<тип>:<объект>:<статус>`;
- адрес — `inbox.AddrIn(req.RemoteIP, сети)`: пустой адрес — отказ;
- отказы: `inbox.ErrNotAuthentic`, `inbox.ErrMalformed`, `inbox.ErrUnavailable`.

**2. Верификатор проходит набор блока** — это тест проекта, а не блока:

```go
func TestAcmeVerifier(t *testing.T) {
	inboxtest.RunVerifierSuite(t, func(_ *testing.T, now func() time.Time) inboxtest.VerifierFixture {
		return inboxtest.VerifierFixture{
			Source:    "acme",
			Verifier:  acme.NewVerifier(testSecret, oldSecret, now), // верификатор проекта
			Sign:      func(n int, at time.Time) inbox.Request { return acme.SignForTest(testSecret, n, at) },
			SignOther: func(n int, at time.Time) inbox.Request { return acme.SignForTest(oldSecret, n, at) },
			Tolerance: 5 * time.Minute, // ноль — время схема не подписывает
		}
	})
}
```

Набор меняет каждый байт тела и заголовков, подставляет мусор, сдвигает время,
проверяет второй секрет, адрес, «дрейф» перечитанного объекта (`Drift`),
владение памятью запроса и параллельные вызовы. Какие поломки он называет —
`inboxtest/suite_verifier_internal_test.go`.

**3. Хранилище — `inboxpg`.** Схему накатывает раннер проекта со своей таблицей
версий `inbox_schema_version`: без `WithTableName` goose пишет в общую
`goose_db_version`, и номера блоков тулкита в ней столкнутся. Блок схему не
применяет — на старте `CheckSchema` сверяет её и называет каждое расхождение.

```go
p, err := goose.NewProvider(goose.DialectPostgres, db, inboxpg.Migrations(),
	goose.WithTableName("inbox_schema_version"))
if err != nil {
	return err
}
if _, err = p.Up(ctx); err != nil {
	return err
}

store := inboxpg.New(pool, map[inbox.SourceName]inboxpg.Handler{"acme": acmeHandler{producer: producer}})
if err := store.CheckSchema(ctx); err != nil {
	return err // миграции не накатаны или схема расходится с ними
}
```

Обработчик получает транзакцию приёма и кладёт внешний эффект сообщением
`outbox` в неё же (ADR-0012, решение 6): отметка, решение и сообщение наружу
коммитятся вместе, а доставку со своими повторами делает очередь.

```go
type acmeHandler struct{ producer *outbox.Producer }

func (h acmeHandler) Handle(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
	var paid struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(ev.Payload, &paid); err != nil {
		return err // не разобрали мы: 503, повтор дойдёт до починенной реплики
	}
	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'paid' WHERE id = $1 AND status = 'pending'`, paid.OrderID)
	if err != nil {
		return postgres.Sanitize(err) // в Detail сырой ошибки — строка заказа
	}
	if tag.RowsAffected() == 0 {
		return nil // заказа нет или он уже оплачен: решение принято
	}
	payload, err := json.Marshal(paid)
	if err != nil {
		return err
	}
	// Ключ события — до 200 байт, с источником он не влезет в ключ outbox: хеш.
	key := sha256.Sum256([]byte(string(ev.Source) + ":" + ev.ID))
	env, err := h.producer.Prepare(outbox.Message{
		Kind:          "order.paid",
		Payload:       payload,
		DedupKey:      hex.EncodeToString(key[:]),
		SchemaVersion: 1,
	})
	if err != nil {
		return err
	}
	_, err = outboxpg.Enqueue(ctx, tx, env)
	return err
}
```

`Accept` держит соединение пула, пока работает обработчик, — поэтому в нём нет
вызовов чужого API. В `WithTx` любая ошибка `Accept` оставляет транзакцию
проекта прерванной: `COMMIT` не пройдёт, и эффект без отметки не закоммитится.

**4. Сервис, ручка и уборка:**

```go
svc := inbox.NewService(store, observer, inbox.Config{
	Sources: map[inbox.SourceName]inbox.SourceConfig{
		"acme": {
			Verifier: verifier,
			Handle:   []inbox.EventType{"order.paid", "order.refunded"},
			Ignore:   []inbox.EventType{"order.viewed"}, // шлют, но не обработаем никогда
			Ack:      inbox.Ack{ContentType: "text/plain; charset=utf-8", Body: []byte("OK")},
		},
	},
	MaxBodyBytes:     256 << 10,
	Retention:        45 * 24 * time.Hour, // не короче окна повторов самого медленного отправителя
	PayloadRetention: 7 * 24 * time.Hour,  // в теле персональные данные
	PurgeBatch:       1000,
})
mux.Handle("POST /webhooks/acme", inboxhttp.New(svc, "acme", inboxhttp.Config{
	RemoteIP:  func(r *http.Request) string { return ratelimithttp.ClientIP(r, trustedProxies) },
	RequestID: reqid.From,
}))
job := scheduler.Job{Name: "inbox_purge", Interval: time.Hour, Run: svc.Purge}
```

`NewService` паникует на негодном `Config` и на источнике без обработчика в
хранилище: ошибка сборки падает на старте. Ручка отвечает классом ошибки без
`Translate` проекта — иначе правило продукта, совпавшее с ошибкой обработчика,
превратило бы 503 в 4xx.

## Обработчик

Обработчик источника — хук хранилища, он получает транзакцию приёма
(ADR-0012, решение 6):

- **внешних вызовов нет**: соединение из пула на время чужого API — очередь на
  весь приём;
- **внешний эффект — сообщение `outbox` в той же транзакции** с ключом дедупа из
  источника и `Event.ID`;
- **отказ по правилу домена — `nil`**: «заказа нет», «уже оплачен» — решение
  принято. Ошибка значит «не решили»: отметка откатится, отправитель повторит;
- **порядок решает машина статусов**: отправители порядок не гарантируют, строки
  предмета — `FOR UPDATE` в порядке, записанном у проекта.

## Что уходит отправителю

| Исход | Когда | Ответ |
|---|---|---|
| `accepted` | новое событие обработано и закоммичено | 200 и `Ack` |
| `duplicate` | ключ есть, отпечаток тот же | 200 и `Ack` |
| `conflict` | ключ есть, отпечаток другой — ошибка вывода ключа; алерт с порогом 1 | 200 и `Ack` |
| `ignored` | тип в `Ignore` | 200 и `Ack`, в базу не ходим |
| `unknown_type` | тип ни в `Handle`, ни в `Ignore` | 503 |
| `in_flight` | тот же ключ сейчас в транзакции | 409 |
| `not_authentic` | верификатор отказал | 400 |
| `malformed` | подлинное, но непригодное | 503 |
| `too_large` | тело больше `MaxBodyBytes` | 413 |
| `error` | база, перечитывание, обработчик | 503 |

**Тип из `Ignore` в `Handle` переводить нельзя без потерь:** на выкате старая
реплика ответит 200 без отметки. Тип, который скоро станут обрабатывать, в
`Ignore` не кладут — пусть 503 держит его у отправителя.

## Тесты проекта

Код проекта тестируется на двойниках блока — отличие от прода в
конструкторах:

- `inboxtest.MemStore` — хранилище с обработчиками источников: дедуп,
  отпечаток, CHECK схемы, `in_flight` на том же ключе во время обработчика,
  моменты как в `timestamptz`; `SetErr` — сбой базы, `Mark` и `Payload` — что
  записано, `CallCount` — «до хранилища не дошли»;
- `inboxtest.HMACVerifier`, `SignHMAC`, `EventBody` — тестовая схема подписи
  для ручки без провайдера. Не боевая;
- `inboxtest.RunStoreSuite` — контрактный набор хранилища: его проходят двойник
  и `inboxpg`.

## Наблюдаемость

`Observer` обязателен: `inbox.LogObserver` для проекта без метрик, счётчик и
гистограмма — `inboxotel`, следующий шаг модуля. Метрики и алерты — ADR-0012,
решение 16. Ни тело, ни заголовки, ни ключ события в лог и метки не попадают.

## Версии

Изменения — [CHANGELOG.md](CHANGELOG.md). Пока `v0.x`: минорный бамп может
ломать API ([VERSIONING.md](../VERSIONING.md)).
