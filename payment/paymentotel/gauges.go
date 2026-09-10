package paymentotel

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/payment"
)

// Имена и единица гейджей — часть контракта: на них стоят алерты потребителя
// (ADR-0004, «Наблюдаемость и алерты»).
const (
	stuckName  = "payment_intents_stuck"
	driftName  = "payment_drift"
	unitIntent = "{intent}"
	attrKind   = "kind"
)

// Snapshot — что потребитель прочитал у сервиса одним заходом.
type Snapshot struct {
	// Stuck — Service.CountStuckPending.
	Stuck int64
	// Drift — Service.Drift как есть. Число в гейдже не больше limit, с которым
	// звали Drift: алерту с порогом 1 этого хватает.
	Drift []payment.DriftRecord
}

// Gauges — снимок сверки за двумя observable gauge. Потокобезопасен; до
// первого Set отдаёт нули.
type Gauges struct {
	mu   sync.RWMutex
	last counts
	reg  metric.Registration
}

// counts — снимок в том виде, в каком его читает коллбэк: одни числа.
type counts struct {
	stuck int64
	// drift после Set не меняется — Set кладёт новую карту, — поэтому коллбэк
	// читает её без копии.
	drift map[payment.DriftKind]int64
	// unknown — записи с родом вне AllDriftKinds: см. doc.go, п. 7.
	unknown int64
}

// NewGauges паникует на nil-метре (как payment.NewService) и возвращает ошибку
// создания инструмента. Оба гейджа — один RegisterCallback: снимок читается
// один раз на scrape, и «зависшие» с «расхождениями» на дашборде из одного
// момента.
func NewGauges(meter metric.Meter) (*Gauges, error) {
	if meter == nil {
		panic("paymentotel.NewGauges: nil meter")
	}
	stuck, err := meter.Int64ObservableGauge(stuckName,
		metric.WithUnit(unitIntent),
		metric.WithDescription("Незакрытые намерения старше порога сверки."),
	)
	if err != nil {
		return nil, fmt.Errorf("paymentotel: инструмент %s: %w", stuckName, err)
	}
	drift, err := meter.Int64ObservableGauge(driftName,
		metric.WithUnit(unitIntent),
		metric.WithDescription("Расхождения книг по роду: деньги и учёт разошлись."),
	)
	if err != nil {
		return nil, fmt.Errorf("paymentotel: инструмент %s: %w", driftName, err)
	}

	g := &Gauges{}
	g.reg, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		c := g.snapshot()
		obs.ObserveInt64(stuck, c.stuck)
		// Ряд на каждый род, в том числе нулевой: пустой ряд на дашборде
		// неотличим от «экспортёр отвалился».
		for _, kind := range payment.AllDriftKinds {
			obs.ObserveInt64(drift, c.drift[kind],
				metric.WithAttributes(attribute.String(attrKind, string(kind))))
		}
		if c.unknown > 0 {
			obs.ObserveInt64(drift, c.unknown)
		}
		return nil
	}, stuck, drift)
	if err != nil {
		return nil, fmt.Errorf("paymentotel: коллбэк гейджей сверки: %w", err)
	}
	return g, nil
}

// Unregister снимает коллбэк: гейджи перестают попадать в scrape, Set после
// этого безвреден. Нужен тому, кто пересобирает сервис в живом процессе.
// Идемпотентен и потокобезопасен — это контракт metric.Registration.
func (g *Gauges) Unregister() error {
	if err := g.reg.Unregister(); err != nil {
		return fmt.Errorf("paymentotel: снять коллбэк гейджей сверки: %w", err)
	}
	return nil
}

// Set кладёт новый снимок целиком; зовёт его отдельная задача планировщика
// потребителя, не scrape и не сверка (CONVENTIONS §6, PATTERNS §8). Записи
// Drift не хранятся — только числа по роду: см. doc.go, п. 6.
func (g *Gauges) Set(s Snapshot) {
	c := counts{stuck: s.Stuck, drift: make(map[payment.DriftKind]int64, len(payment.AllDriftKinds))}
	for _, rec := range s.Drift {
		if slices.Contains(payment.AllDriftKinds, rec.Kind) {
			c.drift[rec.Kind]++
		} else {
			c.unknown++
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = c
}

func (g *Gauges) snapshot() counts {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.last
}
