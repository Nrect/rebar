# Changelog — otelboot

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

## [0.1.0] — 2026-09-10

### Added
- Каркас модуля: `otelboot.Start(ctx, Config) (Providers, error)` — бутстрап
  наблюдаемости одним вызовом. Провайдер метрик на экспортёре Prometheus в
  собственном `prometheus.Registry` (не `DefaultRegisterer`: два инстанса в
  одном процессе иначе падают на дублирующей регистрации), обработчик
  `/metrics` (`promhttp`), ресурс с `service.name`, `service.version` и
  `deployment.environment.name`, observable gauge `build_info{version,commit}=1`
  (meter `rebar.otelboot`), метрики go runtime по `Config.RuntimeMetrics`.
- Трейсинг: пустой `Config.TracesEndpoint` → `noop.NewTracerProvider()`, иначе
  экспортёр OTLP/HTTP с батчем (5 с), `ParentBased(TraceIDRatioBased)` и
  W3C-пропагаторы (`traceparent` + `baggage`). Глобали `otel.SetMeterProvider`
  / `SetTracerProvider` — только по `Config.SetGlobals`, а трейсер и
  пропагаторы — ещё и только для реального провайдера: noop не затирает чужую
  разводку.
- `Config` с проверкой на старте вместо паники (`Start` возвращает ошибку с
  именем поля): `ServiceName` и `Environment` обязательны и по формату
  `[a-z0-9_-]{1,64}`, `SampleRatio ∈ (0,1]` с явной проверкой NaN
  (`TraceIDRatioBased(NaN)` даёт молча пустые трейсы), `TracesEndpoint` —
  http(s) URL (`otlptracehttp` на неразобранном URL молча оставляет умолчание
  `localhost:4318`). Пустые `Version`/`Commit` → `dev`/`unknown`,
  `SampleRatio` 0 → 1.0. Сам адрес приёмника в текст ошибки не попадает.
- `Providers.Shutdown` — идемпотентен (`sync.Once`), сначала сбрасывает батч
  спанов, затем гасит провайдер метрик.
- Подпакет `errtrack` — трекер ошибок, совместимый с Sentry: `Init(dsn,
  environment, release)` (пустой DSN → полный no-op без ошибки),
  `CaptureException`, `CapturePanic(rec, stack)` (стек снимается в defer
  потребителя: собственный стек sentry указывает на middleware, а не на место
  паники), `WrapLogger` (записи уровня Error уходят событием вместе с
  bound-атрибутами `Logger.With`), `SkipKey` / `Skip()` для записей, событие по
  которым уже отправлено. Трекер держит собственный хаб, а не
  `sentry.CurrentHub()`, — глобальное состояние sentry у потребителя не
  трогается.
- Страж импортов `importguard_test.go`: корню разрешены три префикса на одну
  причину «бутстрап otel» (`go.opentelemetry.io/otel`,
  `go.opentelemetry.io/contrib`, `github.com/prometheus/client_golang`),
  `errtrack` — `github.com/getsentry/sentry-go`.
- `doc.go` со списками «Безопасность:» и «Чего нет» в обоих пакетах,
  `README.md` с quickstart.

### Security
- DSN трекера не попадает ни в лог, ни в ошибку: `sentry.NewClient` возвращает
  ошибку разбора с URL целиком (`url.Error`), а в DSN лежит ключ проекта;
  `errtrack.Init` подменяет её своей.
- `/metrics` отдаётся без авторизации — обработчик вешает потребитель, на
  отдельный порт во внутренней сети (`doc.go`, пункт 1; README).
- Toolchain go1.26.6 — та же патч-версия stdlib, что в `mail`: govulncheck
  проверяет ту stdlib, которой собран модуль.
- Транзитивные зависимости подняты выше уязвимых версий: `golang.org/x/text`
  v0.41.0 (GO-2026-5970, бесконечный цикл в `norm`, до которого дотягивается
  `propagation`), `google.golang.org/grpc` v1.82.1 (GO-2026-6061, приходит с
  `otlptrace`), `golang.org/x/net` v0.56.0 (GO-2026-5942, вызова нет, но
  еженедельный vuln-scan на него светит). Версии otel не менялись — семейство
  v1.44.0, как в `mail`.
