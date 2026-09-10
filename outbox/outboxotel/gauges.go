package outboxotel

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/outbox"
)

// Имена и единицы гейджей — часть контракта: на них стоят алерты потребителя
// (ADR-0002, «Наблюдаемость и алерты»). Prometheus-экспортёр припишет _seconds
// к возрасту по единице s.
const (
	pendingName    = "outbox_pending"
	processingName = "outbox_processing"
	failedName     = "outbox_failed"
	unhandledName  = "outbox_unhandled"
	oldestName     = "outbox_oldest_due_age"
)

// Gauges — снимок outbox.Stats за пятью observable gauge. Потокобезопасен; до
// первого Set отдаёт нули.
type Gauges struct {
	mu    sync.RWMutex
	stats outbox.Stats
	reg   metric.Registration
}

// NewGauges паникует на nil-метре и возвращает ошибку создания инструмента.
// Все пять гейджей — ОДИН RegisterCallback: снимок читается один раз на scrape
// и не разъезжается между метриками, иначе «pending» и «возраст» на дашборде
// были бы из разных моментов.
func NewGauges(meter metric.Meter) (*Gauges, error) {
	if meter == nil {
		panic("outboxotel.NewGauges: nil meter")
	}
	g := &Gauges{}
	counts, err := countGauges(meter)
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Float64ObservableGauge(oldestName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Возраст старейшей строки, чей срок уже наступил."),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxotel: инструмент %s: %w", oldestName, err)
	}

	instruments := []metric.Observable{oldest}
	for _, c := range counts {
		instruments = append(instruments, c.gauge)
	}
	g.reg, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		s := g.snapshot()
		for _, c := range counts {
			obs.ObserveInt64(c.gauge, c.value(s))
		}
		obs.ObserveFloat64(oldest, s.OldestDueAge.Seconds())
		return nil
	}, instruments...)
	if err != nil {
		return nil, fmt.Errorf("outboxotel: коллбэк гейджей очереди: %w", err)
	}
	return g, nil
}

// countGauge — гейдж и то, откуда он берёт значение в снимке.
type countGauge struct {
	gauge metric.Int64ObservableGauge
	value func(outbox.Stats) int64
}

func countGauges(meter metric.Meter) ([]countGauge, error) {
	specs := []struct {
		name        string
		description string
		value       func(outbox.Stats) int64
	}{
		{pendingName, "Строк в pending, включая отложенные.", func(s outbox.Stats) int64 { return s.Pending }},
		{processingName, "Строк под арендой.", func(s outbox.Stats) int64 { return s.Processing }},
		{failedName, "Dead-letter до разбора: Purge его не чистит.", func(s outbox.Stats) int64 { return s.Failed }},
		{unhandledName, "Строк с типом, которого нет в реестре воркера.", func(s outbox.Stats) int64 { return s.Unhandled }},
	}
	gauges := make([]countGauge, 0, len(specs))
	for _, spec := range specs {
		gauge, err := meter.Int64ObservableGauge(spec.name,
			metric.WithUnit(unitMessage), metric.WithDescription(spec.description))
		if err != nil {
			return nil, fmt.Errorf("outboxotel: инструмент %s: %w", spec.name, err)
		}
		gauges = append(gauges, countGauge{gauge: gauge, value: spec.value})
	}
	return gauges, nil
}

// Unregister снимает коллбэк: гейджи перестают попадать в scrape, Set после
// этого безвреден. Нужен тому, кто пересобирает воркер в живом процессе.
// Идемпотентен и потокобезопасен — это контракт metric.Registration.
func (g *Gauges) Unregister() error {
	if err := g.reg.Unregister(); err != nil {
		return fmt.Errorf("outboxotel: снять коллбэк гейджей очереди: %w", err)
	}
	return nil
}

// Set кладёт новый снимок; зовётся своей задачей планировщика потребителя —
// не из задачи Drain и НЕ коллбэком, ходящим в базу на каждый scrape: иначе
// частоту запросов к базе задавали бы настройки Prometheus, а не мы
// (CONVENTIONS §6, PATTERNS §8).
func (g *Gauges) Set(s outbox.Stats) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats = s
}

func (g *Gauges) snapshot() outbox.Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.stats
}
