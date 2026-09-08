package otelboot_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/nrect/rebar/otelboot"
)

// Тесты этого файла не параллельны: часть из них читает и ставит глобали otel,
// а они на весь процесс.

// testEndpoint — синтаксически годный адрес приёмника. Спанов тесты не создают,
// поэтому в сеть экспортёр не ходит.
const testEndpoint = "http://127.0.0.1:4318/v1/traces"

func minimalConfig() otelboot.Config {
	return otelboot.Config{ServiceName: "shop", Environment: "production"}
}

// start поднимает провайдеры и гасит их после теста. Shutdown зовётся с
// Background: t.Context() к моменту очистки уже отменён.
func start(t *testing.T, cfg otelboot.Config) otelboot.Providers {
	t.Helper()
	p, err := otelboot.Start(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Shutdown(context.Background())) })
	return p
}

func scrape(t *testing.T, p otelboot.Providers) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	require.Equal(t, 200, rec.Code)
	return rec.Body.String()
}

// metricLine — строка экспозиции с данным именем метрики.
func metricLine(t *testing.T, body, name string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			return line
		}
	}
	t.Fatalf("в /metrics нет метрики %s; отдано:\n%s", name, body)
	return ""
}

func hasGoRuntimeMetrics(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "go_") {
			return true
		}
	}
	return false
}

// keepGlobals снимает и возвращает глобали otel: тест, которому положено их
// ставить, не должен ронять соседние.
func keepGlobals(t *testing.T) {
	t.Helper()
	tracer, meter, propagator := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracer)
		otel.SetMeterProvider(meter)
		otel.SetTextMapPropagator(propagator)
	})
}

// /metrics отдаёт build_info=1 с версией и коммитом в метках — по этой метрике
// на дашборде видно, какая сборка крутится.
func TestStart_MetricsExposeBuildInfo(t *testing.T) {
	cfg := minimalConfig()
	cfg.Version, cfg.Commit = "1.2.3", "abc123"

	line := metricLine(t, scrape(t, start(t, cfg)), "build_info")

	require.Contains(t, line, `version="1.2.3"`)
	require.Contains(t, line, `commit="abc123"`)
	require.True(t, strings.HasSuffix(line, " 1"), "значение build_info всегда 1, получено %q", line)
}

// Пустые Version/Commit — не пустые метки: "dev"/"unknown" читаются на
// дашборде, а пустая строка выглядит как поломка экспортёра.
func TestStart_BuildInfoDefaults(t *testing.T) {
	line := metricLine(t, scrape(t, start(t, minimalConfig())), "build_info")

	require.Contains(t, line, `version="dev"`)
	require.Contains(t, line, `commit="unknown"`)
}

// Ресурс несёт service.name, service.version и окружение: без них телеметрия
// двух сервисов в одном коллекторе неразличима.
func TestStart_ResourceCarriesServiceAndEnvironment(t *testing.T) {
	cfg := minimalConfig()
	cfg.Version = "1.2.3"

	line := metricLine(t, scrape(t, start(t, cfg)), "target_info")

	require.Contains(t, line, `service_name="shop"`)
	require.Contains(t, line, `service_version="1.2.3"`)
	require.Contains(t, line, `deployment_environment_name="production"`)
}

// Runtime-метрики — по флагу: их десятки, и потребитель решает сам.
func TestStart_RuntimeMetricsFollowFlag(t *testing.T) {
	off := minimalConfig()
	require.False(t, hasGoRuntimeMetrics(scrape(t, start(t, off))), "без флага go_* быть не должно")

	on := minimalConfig()
	on.RuntimeMetrics = true
	require.True(t, hasGoRuntimeMetrics(scrape(t, start(t, on))), "с флагом ожидаются go_*")
}

// Негодный конфиг — ошибка на старте с именем поля: потребитель чинит
// конфигурацию по логу, без исходников пакета.
func TestStart_RejectsInvalidConfig(t *testing.T) {
	withRatio := func(r float64) otelboot.Config {
		cfg := minimalConfig()
		cfg.SampleRatio = r
		return cfg
	}
	withEndpoint := func(e string) otelboot.Config {
		cfg := minimalConfig()
		cfg.TracesEndpoint = e
		return cfg
	}

	cases := []struct {
		name  string
		cfg   otelboot.Config
		field string
	}{
		{"пустое имя", otelboot.Config{Environment: "production"}, "Config.ServiceName"},
		{"имя не по формату", otelboot.Config{ServiceName: "Shop Service", Environment: "production"}, "Config.ServiceName"},
		{"пустое окружение", otelboot.Config{ServiceName: "shop"}, "Config.Environment"},
		{"ratio NaN", withRatio(math.NaN()), "Config.SampleRatio"},
		{"ratio +Inf", withRatio(math.Inf(1)), "Config.SampleRatio"},
		{"ratio 2.0", withRatio(2), "Config.SampleRatio"},
		{"ratio -1", withRatio(-1), "Config.SampleRatio"},
		{"endpoint без схемы", withEndpoint("secret-host.internal:4318/v1/traces"), "Config.TracesEndpoint"},
		{"endpoint не http", withEndpoint("grpc://secret-host.internal:4318"), "Config.TracesEndpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := otelboot.Start(t.Context(), tc.cfg)

			require.ErrorContains(t, err, tc.field)
			require.Nil(t, p.Shutdown, "при ошибке провайдеры не отдаются")
			require.NotContains(t, err.Error(), "secret-host", "адрес приёмника в текст ошибки не попадает")
		})
	}
}

// Без endpoint трейсинг — noop, и чужие глобали не тронуты: тест или второй
// инстанс не должен затирать разводку потребителя.
func TestStart_NoEndpointGivesNoopAndKeepsGlobals(t *testing.T) {
	keepGlobals(t)

	tracerBefore, meterBefore := otel.GetTracerProvider(), otel.GetMeterProvider()

	cfg := minimalConfig()
	cfg.SetGlobals = true // даже с опцией noop-трейсер глобаль не занимает
	p := start(t, cfg)

	require.IsType(t, noop.NewTracerProvider(), p.Tracer)
	require.Same(t, tracerBefore, otel.GetTracerProvider(), "noop не ставит глобальный трейсер")
	require.NotSame(t, meterBefore, otel.GetMeterProvider(), "SetGlobals ставит глобальный метр")
}

// Без SetGlobals не ставится ничего, даже при реальном приёмнике.
func TestStart_WithoutSetGlobalsKeepsThem(t *testing.T) {
	keepGlobals(t)

	tracerBefore, meterBefore := otel.GetTracerProvider(), otel.GetMeterProvider()

	cfg := minimalConfig()
	cfg.TracesEndpoint = testEndpoint
	p := start(t, cfg)

	require.IsType(t, &sdktrace.TracerProvider{}, p.Tracer)
	require.Same(t, tracerBefore, otel.GetTracerProvider())
	require.Same(t, meterBefore, otel.GetMeterProvider())
	require.NotContains(t, otel.GetTextMapPropagator().Fields(), "traceparent")
}

// SetGlobals с реальным приёмником ставит оба провайдера и W3C-пропагаторы.
func TestStart_SetGlobals(t *testing.T) {
	keepGlobals(t)

	cfg := minimalConfig()
	cfg.TracesEndpoint = testEndpoint
	cfg.SetGlobals = true
	p := start(t, cfg)

	require.Same(t, p.Tracer, otel.GetTracerProvider())
	require.Same(t, p.Meter, otel.GetMeterProvider())
	require.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent")
}

// Shutdown идемпотентен: graceful shutdown зовёт его из двух мест.
func TestStart_ShutdownIsIdempotent(t *testing.T) {
	cfg := minimalConfig()
	cfg.TracesEndpoint = testEndpoint
	p := start(t, cfg)

	require.NoError(t, p.Shutdown(context.Background()))
	require.NoError(t, p.Shutdown(context.Background()))
}
