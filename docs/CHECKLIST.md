# Чек-лист встраивания `mail`

Один пункт — один шаг от `go get` до алерта в Grafana; подробности по ссылкам.
Порядок важен: без миграции сервис не стартует, без гейджей алертам не о чем гореть.

1. [ ] `go get github.com/nrect/rebar/mail@main` (после тега — `@v0.1.0`) —
   [README, «Установка»](../mail/README.md#установка).
2. [ ] Скопировать `mailpg/schema.sql` в миграции со своим номером (или
   применить `mailpg.Schema` из кода) и на старте звать
   `mailpg.New(pool).CheckSchema(ctx)`: ошибка — стоп процесса с её текстом,
   первая строка говорит, что делать; сам пакет схему не применяет —
   [README, «Миграция»](../mail/README.md#миграция).
3. [ ] `mail.Config`: свои `Kinds`, `From` с SPF/DKIM, `Lease > SendTimeout`,
   `MinSendGap` под квоту провайдера, `Uncertain` по типу писем —
   [README, «Проводка»](../mail/README.md#проводка).
4. [ ] Транспорт: `sesv2.New` (Postbox/SES) или `smtp.New`; ключи из окружения;
   пока провайдера нет — `mail.Unconfigured{}` —
   [README, «Прод без провайдера»](../mail/README.md#прод-без-провайдера).
5. [ ] `mailotel.Wrap` вокруг транспорта и `mailotel.NewGauges`, снимок
   `svc.Stats` → `gauges.Set` после каждого `Deliver` —
   [README, «Наблюдаемость»](../mail/README.md#наблюдаемость).
6. [ ] Два задания планировщика: `Deliver` каждые 30 с, `Purge` раз в час;
   реплики воркера безопасны —
   [README, «Фоновые задания»](../mail/README.md#фоновые-задания).
7. [ ] Алерты в Grafana: «почта застряла», «письма умирают», «провайдер
   отказывает», «крон почты умер» — пороги в
   [README, «Наблюдаемость»](../mail/README.md#наблюдаемость) и
   [ADR-0001](adr/0001-mail.md#наблюдаемость-и-алерты-для-таблиц-потребителя).
8. [ ] Стенд: `sesfake` + Mailpit из `cmd/sesfake/docker-compose.example.yml`,
   транспорт `sesv2` с `AllowInsecureEndpoint` —
   [README, «Стенд»](../mail/README.md#стенд).
9. [ ] Тесты потребителя на двойниках `mailtest`: «отправили → `Deliver` →
   `tr.Sent()` ровно одно» — [README, «Тесты»](../mail/README.md#тесты).
10. [ ] Что делать при `failed`: `rejected` — чинить адрес/письмо, повтор
   вреден; `exhausted` — провайдер лежал дольше окна ретраев, поднять
   `MaxAttempts` или переотправить новым ключом; `uncertain` (только при
   `UncertainPark`) — ручной разбор, письмо могло уйти —
   [ADR-0001, «Доставка»](adr/0001-mail.md#доставка).
