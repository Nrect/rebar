package otelboot

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
)

// Config — параметры бутстрапа наблюдаемости. Имя продукта здесь не зашито:
// сервис, версия и окружение приходят аргументами, поэтому пакет переносится
// в чужой проект без правок.
type Config struct {
	// ServiceName — service.name ресурса. Обязателен, [a-z0-9_-], 1..64 символа.
	ServiceName string

	// Version, Commit — метки build_info. Пусто → "dev" и "unknown".
	Version string
	Commit  string

	// Environment — deployment.environment.name: development|staging|production
	// либо своё [a-z0-9_-]. Обязателен: пустая метка сливает на дашборде прод и
	// dev, а умолчания «правильного» окружения не существует.
	Environment string

	// TracesEndpoint — полный URL приёмника OTLP/HTTP
	// (http://alloy:4318/v1/traces). Пусто → noop-трейсинг.
	TracesEndpoint string

	// SampleRatio ∈ (0,1] — доля корневых трейсов; 0 → 1.0. Child-спаны следуют
	// за родителем (ParentBased).
	SampleRatio float64

	// RuntimeMetrics — метрики go runtime (go_*) в /metrics.
	RuntimeMetrics bool

	// SetGlobals — ставить otel.SetMeterProvider, а для реального трейсера ещё
	// otel.SetTracerProvider и W3C-пропагаторы. По умолчанию false: тест не
	// должен затирать чужие глобали.
	SetGlobals bool
}

// nameRe — общий формат service.name и окружения: метка метрики и значение
// ресурса, поэтому пробелы, юникод и запятые исключены.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// normalized подставляет умолчания и проверяет конфиг. Ошибка называет поле:
// потребитель чинит конфигурацию по логу, без исходников пакета. Паники здесь
// нет — Start возвращает ошибку, и потребитель падает на старте сам.
func (c Config) normalized() (Config, error) {
	if !nameRe.MatchString(c.ServiceName) {
		return Config{}, fmt.Errorf("otelboot: Config.ServiceName %q: ожидается [a-z0-9_-], 1..64 символа", c.ServiceName)
	}
	if !nameRe.MatchString(c.Environment) {
		return Config{}, fmt.Errorf("otelboot: Config.Environment %q: ожидается [a-z0-9_-], 1..64 символа (development|staging|production)", c.Environment)
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.Commit == "" {
		c.Commit = "unknown"
	}
	if c.SampleRatio == 0 {
		c.SampleRatio = 1
	}
	// NaN проверяется отдельным вызовом, а не через отрицание диапазона: любое
	// сравнение с NaN ложно, и NaN добрался бы до TraceIDRatioBased, где даёт
	// молча пустые трейсы вместо отказа на старте.
	if math.IsNaN(c.SampleRatio) || c.SampleRatio <= 0 || c.SampleRatio > 1 {
		return Config{}, fmt.Errorf("otelboot: Config.SampleRatio %v: ожидается (0,1]", c.SampleRatio)
	}
	if err := checkEndpoint(c.TracesEndpoint); err != nil {
		return Config{}, err
	}
	return c, nil
}

// checkEndpoint проверяет endpoint сами: otlptracehttp.WithEndpointURL при
// разборе URL молча пишет в otel-логгер и оставляет умолчание localhost:4318,
// так что кривой адрес без этой проверки выглядел бы как работающий трейсинг.
func checkEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	// Сам адрес в ошибку не печатается: в URL коллектора может оказаться
	// basic-auth.
	badEndpoint := errors.New("otelboot: Config.TracesEndpoint: ожидается http(s) URL OTLP/HTTP, например http://alloy:4318/v1/traces")
	u, err := url.Parse(endpoint)
	if err != nil {
		return badEndpoint
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return badEndpoint
	}
	return nil
}
