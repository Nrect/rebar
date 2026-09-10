# examples/monolith

Потребитель на всех двенадцати модулях сразу — **гейт перед тегами**, а не
витрина. Модули подключены через `replace`: пример проверяет дерево сейчас, а
не опубликованные версии. Что именно проверяется и что не сошлось — `doc.go`.

## Поднять

```bash
docker compose up -d
DATABASE_URL='postgres://monolith:monolith@localhost:5432/monolith?sslmode=disable' \
AUTH_SECRET='замените-на-случайные-32-байта-и-более' \
GOWORK=off go run ./cmd/monolith
```

Миграции накатываются на старте (goose, каталог `migrations/`), схемы адаптеров
там лежат как есть и сверяются `CheckSchema`.

Без почтовика: `SMTP_TRANSPORT=unconfigured` — письма копятся в очереди и
честно падают с `ErrTransportUnconfigured`. Это выбор, а не запасной вариант:
опечатка в настройках SMTP роняет старт.

## Смотреть

- `http://localhost:8025` — Mailpit: письма со ссылками подтверждения и оплаты;
- `http://localhost:8080/metrics` — `build_info`, `cron_*`, `outbox_*`, `emails_*`,
  `payments_total`, `payment_*`; гейджи обновляет задача `gauges_snapshot` раз в
  `GAUGES_TICK` (минута), а не scrape; первый снимок — сразу при старте;
- `http://localhost:8080/healthz` — жив ли процесс.

Ручки: `POST /register`, `GET /confirm?token=`, `POST /signin`, `POST /signout`,
`POST /checkout`, `POST /webhook`, `POST /upload`, `GET /lesson/{id}`.

## Сквозной тест

```bash
colima start
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
GOWORK=off go test -race -count=1 ./...
```

Стенд поднимается сам (Postgres и Mailpit в контейнерах); `-short` пропускает.
