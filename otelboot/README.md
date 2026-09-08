# otelboot

Бутстрап наблюдаемости: один вызов поднимает метрики с экспортёром Prometheus,
`/metrics`, `build_info` и трейсинг OTLP/HTTP. Подпакет `errtrack` — трекер
ошибок, совместимый с Sentry.

Это boot-клей: вся зависимость от otel собрана здесь, чтобы её не было в
остальном коде. Портов и двойников нет.

```
go get github.com/nrect/rebar/otelboot
```

## Старт

```go
p, err := otelboot.Start(ctx, otelboot.Config{
    ServiceName:    "shop",              // обязателен, [a-z0-9_-]
    Version:        buildVersion,        // "" → "dev"
    Commit:         buildCommit,         // "" → "unknown"
    Environment:    "production",        // обязателен, попадает в deployment.environment.name
    TracesEndpoint: os.Getenv("OTEL_TRACES_ENDPOINT"), // "" → noop-трейсинг
    SampleRatio:    1.0,                 // (0,1]; 0 → 1.0
    RuntimeMetrics: true,
    SetGlobals:     true,                // otel.SetMeterProvider и W3C-пропагаторы
})
if err != nil {
    return err // негодный конфиг — падаем на старте, а не на первом запросе
}
defer p.Shutdown(context.Background()) // сбрасывает батч спанов; идемпотентен
```

**`/metrics` отдаётся без авторизации.** Обработчик `p.Metrics` вешает
потребитель — на отдельный порт во внутренней сети, не на публичный роутер:
метрики рассказывают версию сборки, объём трафика и внутренние имена.

```go
go http.ListenAndServe("127.0.0.1:9090", p.Metrics)
```

`p.Meter` и `p.Tracer` передаются библиотекам явно (`otelhttp`, `otelpgx`,
`mailotel`); `SetGlobals` нужен только тем, кто читает глобали otel.

## Трекер ошибок

```go
flush, err := errtrack.Init(os.Getenv("SENTRY_DSN"), "production", buildVersion)
if err != nil {
    return err
}
defer flush(ctx)

log := errtrack.WrapLogger(slog.Default()) // записи уровня Error → событие
```

Пустой DSN — штатный случай: трекер выключен, ошибки нет, все функции пакета —
дешёвые пустышки. В recover-middleware:

```go
defer func() {
    if rec := recover(); rec != nil {
        errtrack.CapturePanic(rec, debug.Stack())
        log.LogAttrs(ctx, slog.LevelError, "panic recovered", errtrack.Skip())
    }
}()
```

`Skip()` гасит дубль: событие по этой панике уже ушло через `CapturePanic`,
богаче и с правильной группировкой.

## Что дальше

Инварианты и «чего нет» — в `doc.go` обоих пакетов. Версии зависимостей ходят
в ногу с остальным тулкитом (otel v1.44.0).
