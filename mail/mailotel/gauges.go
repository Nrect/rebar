package mailotel

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/mail"
)

// Имена и единицы гейджей — часть контракта; Prometheus-экспортёр припишет
// _seconds к возрасту по единице s.
const (
	pendingName = "email_outbox_pending"
	oldestName  = "email_outbox_oldest_pending_age"
	failedName  = "email_outbox_failed"

	unitSeconds = "s"
)

// Gauges — снимок mail.Stats за тремя observable gauge. Потокобезопасен; до
// первого Set отдаёт нули.
type Gauges struct {
	mu    sync.RWMutex
	stats mail.Stats
	reg   metric.Registration
}

// NewGauges паникует на nil-метре (как mail.NewService) и возвращает ошибку
// создания инструмента. Все три гейджа — один RegisterCallback: снимок
// читается один раз на scrape и не разъезжается между метриками.
func NewGauges(meter metric.Meter) (*Gauges, error) {
	if meter == nil {
		panic("mailotel.NewGauges: nil meter")
	}
	pending, err := meter.Int64ObservableGauge(pendingName,
		metric.WithUnit(unitEmail),
		metric.WithDescription("Писем в очереди: pending и sending."),
	)
	if err != nil {
		return nil, fmt.Errorf("mailotel: инструмент %s: %w", pendingName, err)
	}
	oldest, err := meter.Float64ObservableGauge(oldestName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Возраст самого старого неотправленного письма."),
	)
	if err != nil {
		return nil, fmt.Errorf("mailotel: инструмент %s: %w", oldestName, err)
	}
	failed, err := meter.Int64ObservableGauge(failedName,
		metric.WithUnit(unitEmail),
		metric.WithDescription("Терминальных отказов в очереди до Purge."),
	)
	if err != nil {
		return nil, fmt.Errorf("mailotel: инструмент %s: %w", failedName, err)
	}

	g := &Gauges{}
	g.reg, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		s := g.snapshot()
		obs.ObserveInt64(pending, s.Pending)
		obs.ObserveFloat64(oldest, s.OldestPendingAge.Seconds())
		obs.ObserveInt64(failed, s.Failed)
		return nil
	}, pending, oldest, failed)
	if err != nil {
		return nil, fmt.Errorf("mailotel: коллбэк гейджей очереди: %w", err)
	}
	return g, nil
}

// Unregister снимает коллбэк: гейджи перестают попадать в scrape, Set после
// этого безвреден. Нужен тому, кто пересобирает сервис в живом процессе;
// долгоживущему воркеру звать его незачем. Идемпотентен и потокобезопасен —
// это контракт metric.Registration.
func (g *Gauges) Unregister() error {
	if err := g.reg.Unregister(); err != nil {
		return fmt.Errorf("mailotel: снять коллбэк гейджей очереди: %w", err)
	}
	return nil
}

// Set кладёт новый снимок; зовётся потребителем после прогона Deliver, а не
// коллбэком на каждый scrape (CONVENTIONS §6).
func (g *Gauges) Set(s mail.Stats) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats = s
}

func (g *Gauges) snapshot() mail.Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.stats
}
