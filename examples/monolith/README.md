# examples/monolith

Потребитель на всех двенадцати модулях сразу — **гейт перед тегами**, а не
витрина. Модули подключены через `replace`: пример проверяет дерево сейчас, а
не опубликованные версии. Что именно проверяется и что не сошлось — `doc.go`.

## Поднять

```bash
docker compose up -d
set -a; . ./stand.env; set +a
AUTH_SECRET="$(openssl rand -base64 48)" GOWORK=off go run ./cmd/monolith
```

`stand.env` — окружение этого стенда. Послабления в нём записаны словом (http
без `Secure` у куки, Mailpit без TLS и пароля): код их по умолчанию не даёт, и
забытая переменная предохранитель не снимает. Умолчаний нет у того, без чего
прод неверен: `ENVIRONMENT`, `DATABASE_URL`, `AUTH_SECRET`, `BASE_URL`,
`MAIL_FROM`, `MAIL_DOMAIN`, `SMTP_HOST` и учётки SMTP (либо `SMTP_AUTH=none`);
все забытые называются одним списком на старте.

Миграции накатываются на старте (goose, каталог `migrations/`), схемы адаптеров
там лежат как есть и сверяются `CheckSchema` — на старте и в `/readyz`.

`DATABASE_URL` — прямо в Postgres либо через пулер в режиме `session`. В
режиме `transaction` ключ `pglock` у сверки платежей не держится, и две
реплики сверяют одновременно (ADR-0008, «Условие развёртывания»).

Без почтовика: `SMTP_TRANSPORT=unconfigured` — письма копятся в очереди и
честно падают с `ErrTransportUnconfigured`. Это выбор, а не запасной вариант:
опечатка в настройках SMTP роняет старт.

## Смотреть

- `http://localhost:8025` — Mailpit: письма со ссылками подтверждения и оплаты;
- `http://127.0.0.1:9090/metrics` — `build_info`, `cron_*` (в том числе
  `cron_lock_total` — сверка платежей под `pglock`), `outbox_*`, `emails_*`,
  `payments_total`, `payment_*`; гейджи обновляет задача `gauges_snapshot` раз в
  `GAUGES_TICK` (минута), а не scrape; первый снимок — сразу при старте;
- `http://127.0.0.1:9090/healthz` — жив ли процесс;
- `http://127.0.0.1:9090/readyz` — можно ли слать трафик: 503 до старта, с
  начала остановки и когда схема блока не сходится (причина — в логе, Warn).

Служебные ручки — только на `INTERNAL_ADDR` (`127.0.0.1:9090`): `/metrics`
отдаётся без авторизации. Публичный порт `ADDR` (`:8080`) держит ручки
приложения: `POST /register`, `GET /confirm?token=`, `POST /signin`,
`POST /signout`, `POST /checkout`, `POST /webhook`, `POST /upload`,
`GET /lesson/{id}`.

Логи — JSON в stdout, уровень `LOG_LEVEL`. На SIGTERM `/readyz` отвечает 503,
приём запросов закрывается, задачи дописывают идущий прогон, затем пул и
телеметрия; у каждого шага свой бюджет (`process.go`, docs/CONSUMER.md, §5).

## Сквозной тест

```bash
colima start
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
GOWORK=off go test -race -count=1 ./...
```

Стенд поднимается сам (Postgres и Mailpit в контейнерах); `-short` пропускает.
