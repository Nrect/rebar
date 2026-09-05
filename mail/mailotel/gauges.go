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
	if _, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		s := g.snapshot()
		obs.ObserveInt64(pending, s.Pending)
		obs.ObserveFloat64(oldest, s.OldestPendingAge.Seconds())
		obs.ObserveInt64(failed, s.Failed)
		return nil
	}, pending, oldest, failed); err != nil {
		return nil, fmt.Errorf("mailotel: коллбэк гейджей очереди: %w", err)
	}
	return g, nil
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
