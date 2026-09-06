# mail — транзакционная почта: outbox в Postgres, доставка с ретраями

Письмо ложится строкой в таблицу `email_outbox` в базе вашего приложения, и
ответ клиенту не ждёт провайдера. Фоновое задание `Deliver` забирает пачку,
шлёт через транспорт (SMTP либо SES-совместимый HTTP — Yandex Cloud Postbox и
AWS SES одним адаптером) и повторяет по экспоненте с джиттером. Гарантия:
строка в outbox есть ⇒ письмо либо уйдёт, либо станет видимым `failed` с
метрикой, но не потеряется.

Чего пакет не делает (шаблоны, рассылки, вложения, вебхуки, DKIM) — список с
причинами в [doc.go](doc.go); проект и мотивы — [ADR-0001](../docs/adr/0001-mail.md);
чек-лист встраивания — [docs/CHECKLIST.md](../docs/CHECKLIST.md).

## Установка

```bash
go get github.com/nrect/rebar/mail@main   # после тега — @v0.1.0
```

Модуль на пакет: в проект приезжают зависимости только почты, а бамп версии
почты не трогает другие пакеты тулкита ([VERSIONING.md](../VERSIONING.md)).

## Миграция

Скопируйте схему в каталог миграций со своим номером. Файл в формате goose
(`-- +goose Up` / `-- +goose Down`), менять его не нужно:

```bash
cp "$(go list -m -f '{{.Dir}}' github.com/nrect/rebar/mail)/mailpg/schema.sql" migrations/0042_email_outbox.sql
```

Если миграции применяются из кода, тот же файл доступен строкой
`mailpg.Schema`. Пакет схему **не применяет** — автомиграция из библиотеки
даёт две правды о схеме, требует DDL-прав у приложения и гонит реплики на
старте. Вместо этого на старте зовите `store.CheckSchema(ctx)`: он сверяет
колонки, CHECK-ограничения и индексы со `schema.sql` и возвращает одну ошибку
со всеми расхождениями, первая строка — что делать. Свои колонки в таблицу
добавлять можно, расхождением это не считается.

## Проводка

```go
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
	return err
}
store := mailpg.New(pool)
if err := store.CheckSchema(ctx); err != nil {
	return err // миграция не применена или расходится со schema.sql
}

// Транспорт: Yandex Cloud Postbox; для AWS SES — https://email.<region>.amazonaws.com.
tr, err := sesv2.New(sesv2.Config{
	Endpoint:        "https://postbox.cloud.yandex.net",
	Region:          "ru-central1",
	AccessKeyID:     os.Getenv("POSTBOX_ACCESS_KEY_ID"),
	SecretAccessKey: os.Getenv("POSTBOX_SECRET_KEY"),
})
if err != nil {
	return err
}

// Метрики: счётчик отправок вокруг транспорта и три гейджа очереди.
meter := otel.Meter("github.com/nrect/rebar/mail")
metered, err := mailotel.Wrap(tr, meter)
if err != nil {
	return err
}
gauges, err := mailotel.NewGauges(meter)
if err != nil {
	return err
}

svc := mail.NewService(store, metered, nil, cfg) // nil — стоп-лист ведёт провайдер
```

Другие транспорты: `smtp.New(smtp.Config{…})` — STARTTLS обязателен по
умолчанию, открытый текст только с `AllowPlaintext`; `mail.Unconfigured{}` —
провайдера ещё нет (см. «Прод без провайдера»). `NewService` паникует на
nil-порте и негодном `Config`: ошибка конфигурации падает на старте, а не на
первом письме. Рекомендованный `Config`:

```go
cfg := mail.Config{
	From:            mail.Address{Email: "noreply@example.ru", Name: "Пример"}, // домен с SPF/DKIM; письмо From не задаёт
	Kinds:           []mail.Kind{"verify", "reset"},                            // свой закрытый набор: это метка метрики
	MessageIDDomain: "example.ru",                                              // домен отправителя

	MaxAttempts: 8,                                                    // с Backoff ниже — до часа ретраев, потом failed(exhausted)
	Backoff:     mail.Backoff{Base: 30 * time.Second, Max: time.Hour}, // экспонента с полным джиттером: нет волны повторов
	Lease:       2 * time.Minute,                                      // аренда строки; обязана быть больше SendTimeout
	SendTimeout: 30 * time.Second,                                     // ctx одной попытки
	BatchSize:   50,                                                   // строк за прогон Deliver
	MinSendGap:  time.Second,                                          // квота Postbox — 1 письмо/с

	Retention:    30 * 24 * time.Hour, // терминальные строки до Purge (тело уже стёрто)
	MaxBodyBytes: 512 << 10,           // Text + HTML
	Uncertain:    mail.UncertainRetry, // исход прошлой попытки неизвестен → слать снова
}
```

`Uncertain`: для ссылок подтверждения дубль безвреден — `UncertainRetry`. Для
чеков, где дубль — претензия, заведите отдельный `Kind` и отдельный сервис с
`UncertainPark`: такие строки уходят в `failed(uncertain)` на ручной разбор.

## Отправка

```go
res, err := svc.Enqueue(ctx, mail.Message{
	Kind:     "verify",
	To:       mail.Address{Email: user.Email, Name: user.Name},
	Subject:  "Подтвердите почту",
	Text:     "Ссылка для подтверждения: " + link, // обязателен; HTML — необязательная альтернатива
	DedupKey: fmt.Sprintf("verify:%x", sha256.Sum256([]byte(rawToken))),
	NotAfter: tokenExpiresAt, // TTL токена: позже письмо станет expired и не уйдёт
})
switch {
case errors.Is(err, mail.ErrKeyReused):
	return err // тот же ключ на другое письмо — ошибка в коде вызывающего, не повторять
case errors.Is(err, mail.ErrUnavailable):
	return err // база недоступна, письмо не записано: клиенту 5xx
case err != nil:
	return err // ErrInvalidMessage, ErrBadKind, ErrKeyInvalid — письмо собрано неверно
}
if res.Outcome == mail.OutcomeDuplicate {
	// повтор того же письма под тем же ключом: строка та же, второй не появилось
}
```

Ключ выводится из факта, а не из времени: `verify:` + sha256(токена),
`receipt:` + id платежа. Повтор с тем же ключом и тем же письмом — успех
(`OutcomeDuplicate`), с другим письмом — `ErrKeyReused`, громко.

**В транзакции бизнес-факта** — когда письмо следствие денег и не должно
существовать без записи, породившей его:

```go
env, err := svc.Prepare(msg) // валидация и отпечаток; хранилища не касается
if err != nil {
	return err
}
tx, err := pool.Begin(ctx)
if err != nil {
	return err
}
defer tx.Rollback(ctx)
// … INSERT бизнес-факта в той же tx …
if _, err = store.WithTx(tx).Enqueue(ctx, env); err != nil {
	return err
}
return tx.Commit(ctx)
```

## Фоновые задания

Два задания планировщика: `Deliver` каждые 30 с, `Purge` раз в час. После
прогона `Deliver` обновите снимок гейджей. Если планировщика нет — `time.Ticker`:

```go
deliver, purge := time.NewTicker(30*time.Second), time.NewTicker(time.Hour)
defer deliver.Stop()
defer purge.Stop()
for {
	select {
	case <-ctx.Done():
		return
	case <-deliver.C:
		// Ошибка прогона — только сбой Claim/Finish (база); отказ провайдера — исход строки.
		if _, err := svc.Deliver(ctx); err != nil {
			slog.ErrorContext(ctx, "mail deliver", "err", err) // текст без адресов и тел
		}
		if stats, err := svc.Stats(ctx); err == nil {
			gauges.Set(stats)
		}
	case <-purge.C:
		if _, err := svc.Purge(ctx); err != nil {
			slog.ErrorContext(ctx, "mail purge", "err", err)
		}
	}
}
```

Несколько реплик воркера безопасны: `Claim` берёт строки `FOR UPDATE SKIP
LOCKED` с арендой `Lease`, строку упавшего воркера заберут после её истечения.
Доставка at-least-once: падение между отправкой и записью исхода даёт второе
письмо ([doc.go](doc.go), п. 5).

## Прод без провайдера

Провайдер ещё не заведён — соберите сервис с `mail.Unconfigured{}` вместо nil
и вместо «логирующего» транспорта (ссылки с токенами в логах). `Deliver` с ним
очередь не трогает, попытки не тратятся: письма ждут в `pending`, гейдж
возраста растёт, алерт «почта застряла» горит — так и задумано, это честное
состояние. Когда провайдер появится, замените транспорт и перезапустите:
очередь уйдёт сама (письма с истёкшим `NotAfter` станут `expired`).

## Стенд

На dev/stage боевой HTTP-транспорт не подменяется SMTP: цепочка
`backend → sesv2 → sesfake → SMTP → Mailpit`. [cmd/sesfake](cmd/sesfake) —
SES v2-фейк с релеем в Mailpit, письма читаются в его веб-ящике;
[docker-compose.example.yml](cmd/sesfake/docker-compose.example.yml) поднимает
оба. Транспорт стенда — тот же `sesv2`, но по http внутри docker-сети:

```go
tr, err := sesv2.New(sesv2.Config{
	Endpoint:              "http://sesfake:8080",
	Region:                "ru-central1",
	AccessKeyID:           os.Getenv("SESFAKE_ACCESS_KEY_ID"),
	SecretAccessKey:       os.Getenv("SESFAKE_SECRET"), // sesfake сверяет подпись, если запущен с тем же -secret
	AllowInsecureEndpoint: true,                        // http не на loopback: только стенд
})
```

## Тесты

Двойники в [mailtest](mailtest): `NewMemStore` (уникальность ключа, аренда,
стирание тела), `NewTransport` (записывает конверты; `RejectFor`/`FailFor` —
отказы по адресу), `NewMemSuppressor` (стоп-лист в памяти), `NewSESServer(t)`
(фейк SES v2 для адаптера `sesv2` без Docker). Тест потребителя:

```go
func TestRegister_SendsVerifyEmail(t *testing.T) {
	store := mailtest.NewMemStore()
	tr := mailtest.NewTransport()
	svc := mail.NewService(store, tr, nil, cfg)

	_, err := svc.Enqueue(ctx, msg)
	require.NoError(t, err)
	_, err = svc.Deliver(ctx)
	require.NoError(t, err)
	require.Len(t, tr.Sent(), 1) // ровно одно письмо
}
```

## Наблюдаемость

Инструменты `mailotel` и их вид после Prometheus-экспортёра:

| Метрика | Тип | Что |
|---|---|---|
| `emails_sent_total{type,result}` (инструмент `emails_sent`) | counter | результат каждой попытки транспорта; считается в воркере, а не в HTTP-запросе. `type` — `Kind`, `result` — `ok` / `rejected` (провайдер отказал определённо) / `error` (ответа нет, повтор будет) |
| `email_outbox_pending` | gauge | строк в pending/sending |
| `email_outbox_oldest_pending_age_seconds` (инструмент `email_outbox_oldest_pending_age`, единица `s`) | gauge | возраст самой старой неотправленной; ловит «воркер жив, ничего не уходит» |
| `email_outbox_failed` | gauge | терминальных отказов до Purge |
| `cron_*{job="mail_deliver"}`, `{job="mail_purge"}` | из планировщика потребителя | прогоны, длительность, последний успех |

Гейджи читают снимок `Stats`, который вы кладёте в `gauges.Set` после каждого
прогона `Deliver`; запроса в базу на каждый scrape нет.

Алерты ([ADR-0001](../docs/adr/0001-mail.md), «Наблюдаемость и алерты»):

| Алерт | Условие |
|---|---|
| «почта застряла» (page) | `email_outbox_oldest_pending_age_seconds > 600` в течение 10 мин |
| «письма умирают» | `increase(email_outbox_failed[1h]) > 0`; разбор по аудиту потребителя: kind, усечённый адрес, причина — без тела |
| «провайдер отказывает» | доля `rate(emails_sent_total{result="rejected"})` > 5 % |
| «крон почты умер» | `time() - cron_last_success{job="mail_deliver"} > 180` при интервале 30 с |

## Безопасность

Тело письма — секрет (в нём ссылка с токеном): стирается после отправки, не
логируется, не попадает в ошибки; ключи провайдера и адреса в логи и метки
метрик не уходят. Полный список инвариантов — [doc.go](doc.go), «Безопасность:»;
как чинится уязвимость — [SECURITY.md](../SECURITY.md).

## Версии

Изменения — [CHANGELOG.md](CHANGELOG.md). Пока `v0.x`: минорный бамп может
ломать API, теги вида `mail/v0.1.0` ([VERSIONING.md](../VERSIONING.md)).
