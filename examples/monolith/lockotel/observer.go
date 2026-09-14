// Package lockotel — pglock.Observer на OpenTelemetry metric API: счётчик
// cron.lock{job,result}, в Prometheus — cron_lock_total.
//
// Готового наблюдателя у pglock нет, его пишет потребитель (postgres/pglock,
// doc.go). Каталог назван *otel не для вида: otel законен только в таких
// каталогах (depguard в корне репозитория), а ядро примера держит
// TestGuard_OtelStaysInBoot.
package lockotel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/postgres/pglock"
)

// Имя, единица и метки — контракт с алертом «ключ застрял» (doc.go примера).
const (
	attemptsName = "cron.lock"
	unitAttempt  = "{attempt}"
	attrJob      = "job"
	attrResult   = "result"
)

// observer — pglock.Observer: cron.lock{job,result}.
type observer struct{ attempts metric.Int64Counter }

var _ pglock.Observer = (*observer)(nil)

// NewObserver паникует на nil-метре и пустом списке задач: без задач не
// заведётся ни одного ряда, и алерт «ключ застрял» молчал бы всегда. Ошибка
// создания инструмента возвращается, как у paymentotel.NewObserver.
//
// КАЖДЫЙ РЯД РОЖДАЕТСЯ НУЛЁМ: все пары jobs × pglock.AllResults заводятся при
// сборке (CONVENTIONS §6). Ряд, появившийся сразу единицей, increase() не
// видит.
func NewObserver(meter metric.Meter, jobs ...string) (pglock.Observer, error) {
	if meter == nil {
		panic("lockotel.NewObserver: nil meter")
	}
	if len(jobs) == 0 {
		panic("lockotel.NewObserver: no jobs")
	}
	attempts, err := meter.Int64Counter(attemptsName,
		metric.WithUnit(unitAttempt),
		metric.WithDescription("Попытки взять ключ pglock по задаче и исходу."),
	)
	if err != nil {
		return nil, fmt.Errorf("lockotel: инструмент %s: %w", attemptsName, err)
	}
	o := &observer{attempts: attempts}
	for _, job := range jobs {
		for _, result := range pglock.AllResults {
			o.add(context.Background(), job, result, 0)
		}
	}
	return o, nil
}

// Outcome считает исход попытки; job и result — закрытые наборы: имя задачи
// примера и pglock.AllResults.
func (o *observer) Outcome(ctx context.Context, job string, result pglock.Result) {
	o.add(ctx, job, result, 1)
}

func (o *observer) add(ctx context.Context, job string, result pglock.Result, n int64) {
	o.attempts.Add(ctx, n, metric.WithAttributes(
		attribute.String(attrJob, job),
		attribute.String(attrResult, string(result)),
	))
}
