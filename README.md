# rebar

Тулкит переносимых production-ready пакетов на Go. Один пакет — одна
предметная область, ноль знаний о конкретном проекте, копируется в чужой
сервис каталогом или подключается модулем. Уязвимость чинится здесь один раз
и обновляется во всех проектах бампом версии.

Имя — арматура: стальные прутья внутри бетона. Снаружи не видно, но именно
они держат здание. Пакеты отсюда так же стоят внутри любого проекта и не
носят его имени: к ЛайфУроку (первому потребителю) они не привязаны и не
должны привязываться.

## Жители

| Пакет | Модуль | Что | Статус |
|---|---|---|---|
| `mail/` | `github.com/nrect/rebar/mail` | транзакционная почта: outbox в Postgres потребителя, доставка с ретраями, транспорт за портом, стоп-лист; quickstart — [mail/README.md](mail/README.md) | реализован, готовится тег v0.1.0; проект — [ADR-0001](docs/adr/0001-mail.md) |
| &nbsp;&nbsp;`mail/smtp/` | подпакет `mail` | транспорт SMTP на go-mail, STARTTLS обязателен по умолчанию | реализован |
| &nbsp;&nbsp;`mail/sesv2/` | подпакет `mail` | транспорт SES v2-совместимого HTTP API (Yandex Cloud Postbox, AWS SES), SigV4 на stdlib | реализован |
| &nbsp;&nbsp;`mail/mailpg/` | подпакет `mail` | хранилище outbox на pgx/v5: `schema.sql`, `WithTx`, `CheckSchema` на старте | реализован |
| &nbsp;&nbsp;`mail/mailotel/` | подпакет `mail` | наблюдаемость: декоратор транспорта со счётчиком `emails_sent{type,result}` и три гейджа очереди (OpenTelemetry metric API) | реализован |
| &nbsp;&nbsp;`mail/mailtest/` | подпакет `mail` | двойники портов для тестов потребителя и фейк SES v2 без Docker | реализован |
| &nbsp;&nbsp;`mail/cmd/sesfake/` | подпакет `mail` | SES v2-фейк для dev/stage с релеем в Mailpit | реализован |
| `payment/` | — | покупка: намерение, зачисление по вебхуку, возврат, сверка | переезжает из `lifeurok-backend/internal/payment` отдельной задачей |
| `entitlement/` | — | права доступа с кэшем и fail-closed | переезжает из `lifeurok-backend/internal/entitlement` отдельной задачей |

## Правила

- [CONVENTIONS.md](CONVENTIONS.md) — как оформляется пакет и что в нём обязательно.
- [VERSIONING.md](VERSIONING.md) — модуль на пакет, теги `<pkg>/vX.Y.Z`, что считается ломающим.
- [SECURITY.md](SECURITY.md) — как чинится уязвимость и как она доезжает до проектов.
- [docs/CHECKLIST.md](docs/CHECKLIST.md) — чек-лист встраивания пакета `mail` в проект: от `go get` до алертов.

## Команды

```bash
make ci        # lint + race-тесты + govulncheck по всем модулям (то же, что в CI)
make test      # только тесты
make modules   # список модулей репозитория
```

Локально модули связаны `go.work`; в CI каждый модуль собирается сам по себе,
как его увидит потребитель.
