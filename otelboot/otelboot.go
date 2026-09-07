package otelboot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// meterName — scope метрик самого пакета; в Prometheus уезжает меткой
// otel_scope_name.
const meterName = "rebar.otelboot"

// batchTimeout — как долго спаны ждут отправки в батче.
const batchTimeout = 5 * time.Second

// Providers — то, что бутстрап отдаёт потребителю. Meter и Tracer передаются
// библиотекам явно; Metrics потребитель вешает сам (см. doc.go, пункт 1).
type Providers struct {
	Meter   metric.MeterProvider
	Tracer  trace.TracerProvider
	Metrics http.Handler

	// Shutdown обязателен к вызову при остановке: сбрасывает батч спанов и
	// снимает коллбэки метрик. Идемпотентен.
	Shutdown func(ctx context.Context) error
}

// Start поднимает провайдеры наблюдаемости по конфигу. Ошибка означает негодный
// конфиг или неподнявшийся экспортёр — потребителю положено упасть на старте.
func Start(ctx context.Context, cfg Config) (Providers, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return Providers{}, err
	}
	res, err := newResource(cfg)
	if err != nil {
		return Providers{}, err
	}
	meterProvider, handler, err := startMeter(ctx, cfg, res)
	if err != nil {
		return Providers{}, err
	}
	tracerProvider, stopTracer, err := startTracer(ctx, cfg, res)
	if err != nil {
		_ = meterProvider.Shutdown(ctx)
		return Providers{}, err
	}
	if cfg.SetGlobals {
		otel.SetMeterProvider(meterProvider)
	}

	var once sync.Once
	var shutdownErr error
	return Providers{
		Meter:   meterProvider,
		Tracer:  tracerProvider,
		Metrics: handler,
		Shutdown: func(ctx context.Context) error {
			// Трейсер первым: сначала выпустить накопленные спаны, потом гасить
			// метрики. Once делает повторный вызов дешёвым и без ошибки —
			// graceful shutdown часто зовёт его из двух мест.
			once.Do(func() {
				shutdownErr = errors.Join(stopTracer(ctx), meterProvider.Shutdown(ctx))
			})
			return shutdownErr
		},
	}, nil
}

// newResource — общий ресурс метрик и трейсов.
func newResource(cfg Config) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.Version),
		semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("otelboot: сборка ресурса: %w", err)
	}
	return res, nil
}

// startMeter поднимает провайдер метрик на экспортёре Prometheus и отдаёт
// обработчик /metrics.
func startMeter(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, http.Handler, error) {
	// СОБСТВЕННЫЙ РЕЕСТР, НЕ prometheus.DefaultRegisterer. Глобальный реестр
	// делает два инстанса в одном процессе (тесты, стенд рядом с бэкендом)
	// взаимоисключающими: второй экспортёр падает на дублирующей регистрации.
	registry := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, nil, fmt.Errorf("otelboot: экспортёр prometheus: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	if err := registerBuildInfo(provider, cfg); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, nil, err
	}
	if cfg.RuntimeMetrics {
		if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
			_ = provider.Shutdown(ctx)
			return nil, nil, fmt.Errorf("otelboot: runtime-метрики: %w", err)
		}
	}
	return provider, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), nil
}

// registerBuildInfo публикует build_info{version,commit}=1 — приём
// kube_pod_info: значение всегда 1, полезное лежит в метках, и на дашборде
// видно, какая версия крутится и когда задеплоилась.
func registerBuildInfo(provider metric.MeterProvider, cfg Config) error {
	meter := provider.Meter(meterName)
	gauge, err := meter.Int64ObservableGauge("build_info",
		metric.WithDescription("Build metadata of the running binary (value is always 1)"))
	if err != nil {
		return fmt.Errorf("otelboot: инструмент build_info: %w", err)
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(gauge, 1, metric.WithAttributes(
			attribute.String("version", cfg.Version),
			attribute.String("commit", cfg.Commit),
		))
		return nil
	}, gauge)
	if err != nil {
		return fmt.Errorf("otelboot: коллбэк build_info: %w", err)
	}
	return nil
}

// startTracer отдаёт noop без endpoint и реальный OTLP/HTTP-провайдер с ним.
// Глобали и пропагаторы ставятся только для реального провайдера: noop не
// затирает чужую разводку (doc.go, пункт 3).
func startTracer(ctx context.Context, cfg Config, res *resource.Resource) (trace.TracerProvider, func(context.Context) error, error) {
	if cfg.TracesEndpoint == "" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.TracesEndpoint))
	if err != nil {
		return nil, nil, fmt.Errorf("otelboot: экспортёр OTLP/HTTP: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(batchTimeout)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	if cfg.SetGlobals {
		otel.SetTracerProvider(provider)
		// W3C traceparent + baggage: входящий контекст принимаем, исходящий
		// передаём — без этого otelhttp на обоих концах строит разные трейсы.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}))
	}
	return provider, provider.Shutdown, nil
}
