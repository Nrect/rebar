package schedulerotel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nrect/rebar/scheduler"
)

// Result — исход прогона в метке result. Набор закрыт: на этих строках стоят
// алерты потребителя, и смена любой из них ломающая (VERSIONING).
type Result string

const (
	ResultOK Result = "ok"
	// ResultError — задача вернула ошибку: обычная жизнь фоновой работы.
	ResultError Result = "error"
	// ResultPanic — задача паниковала: это баг, а не отказ внешней системы.
	ResultPanic Result = "panic"
)

// AllResults — полный список; держит guard-тест.
var AllResults = []Result{ResultOK, ResultError, ResultPanic}

// Имена и единицы инструментов — часть контракта (см. doc.go).
const (
	runsName        = "cron.runs"
	durationName    = "cron.duration"
	processedName   = "cron.processed"
	lastSuccessName = "cron.last_success_timestamp"

	unitRun     = "{run}"
	unitItem    = "{item}"
	unitSeconds = "s"
)

// Метки: оба набора закрыты, см. doc.go, п. 1.
const (
	attrJob    = "job"
	attrResult = "result"
)

// Observer — scheduler.Observer на metric API. Потокобезопасен: Finished
// зовут горутины задач, коллбэк гейджа — экспортёр на scrape.
type Observer struct {
	runs      metric.Int64Counter
	duration  metric.Float64Histogram
	processed metric.Int64Counter

	mu sync.RWMutex
	// lastSuccess — задача → время последнего успеха; кормит гейдж.
	lastSuccess map[string]time.Time
}

var _ scheduler.Observer = (*Observer)(nil)

// NewObserver паникует на nil-метре (как scheduler.New — на nil-наблюдателе):
// ошибка сборки обязана падать на старте. Ошибка создания инструмента
// возвращается — метрик у потребителя может не быть, а задачи выполнять надо.
//
// Провайдер приходит параметром, а не берётся из otel-глобали: коллбэк
// наблюдаемого гейджа регистрируется РОВНО ОДИН РАЗ и навсегда привязан к
// тому провайдеру, у которого зарегистрирован.
func NewObserver(meter metric.Meter) (scheduler.Observer, error) {
	if meter == nil {
		panic("schedulerotel.NewObserver: meter must not be nil")
	}
	o := &Observer{lastSuccess: make(map[string]time.Time)}
	if err := o.instruments(meter); err != nil {
		return nil, err
	}
	if err := o.registerLastSuccess(meter); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Observer) instruments(meter metric.Meter) error {
	var err error
	if o.runs, err = meter.Int64Counter(runsName,
		metric.WithUnit(unitRun),
		metric.WithDescription("Прогоны задач по имени и исходу."),
	); err != nil {
		return fmt.Errorf("schedulerotel: инструмент %s: %w", runsName, err)
	}
	if o.duration, err = meter.Float64Histogram(durationName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Длительность прогона задачи."),
	); err != nil {
		return fmt.Errorf("schedulerotel: инструмент %s: %w", durationName, err)
	}
	if o.processed, err = meter.Int64Counter(processedName,
		metric.WithUnit(unitItem),
		metric.WithDescription("Элементов обработано задачами."),
	); err != nil {
		return fmt.Errorf("schedulerotel: инструмент %s: %w", processedName, err)
	}
	return nil
}

// registerLastSuccess публикует гейдж последнего успеха: см. doc.go, п. 2.
func (o *Observer) registerLastSuccess(meter metric.Meter) error {
	gauge, err := meter.Float64ObservableGauge(lastSuccessName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Unix-время последнего успешного прогона, по задаче."),
	)
	if err != nil {
		return fmt.Errorf("schedulerotel: инструмент %s: %w", lastSuccessName, err)
	}
	_, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		o.mu.RLock()
		defer o.mu.RUnlock()
		for job, at := range o.lastSuccess {
			if at.IsZero() {
				// UnixNano() нулевого времени — огромное отрицательное число:
				// алерт увидел бы «последний успех в 1754 году» и молчал.
				continue
			}
			obs.ObserveFloat64(gauge, unixSeconds(at), metric.WithAttributes(attribute.String(attrJob, job)))
		}
		return nil
	}, gauge)
	if err != nil {
		return fmt.Errorf("schedulerotel: коллбэк гейджа %s: %w", lastSuccessName, err)
	}
	return nil
}

// Started засеивает ряд гейджа моментом старта — но не перезаписывает уже
// случившийся успех: наблюдателя может делить второй планировщик, и его
// старт не должен откатывать чужую метку назад.
func (o *Observer) Started(jobs []string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, job := range jobs {
		if _, ok := o.lastSuccess[job]; !ok {
			o.lastSuccess[job] = at
		}
	}
}

// Finished считает прогон: счётчик по исходу, длительность, обработанные
// элементы; успех двигает гейдж последнего успеха.
func (o *Observer) Finished(ctx context.Context, run scheduler.Run) {
	job := attribute.String(attrJob, run.Job)
	o.runs.Add(ctx, 1, metric.WithAttributes(job, attribute.String(attrResult, string(classify(run)))))
	o.duration.Record(ctx, run.Elapsed.Seconds(), metric.WithAttributes(job))
	// Ноль пускаем — ряд обязан существовать до первого обработанного
	// элемента, иначе rate() на нём даёт NoData. Отрицательное от кривой
	// задачи — нет: счётчик монотонен по определению.
	if run.Processed >= 0 {
		o.processed.Add(ctx, int64(run.Processed), metric.WithAttributes(job))
	}
	if run.Err == nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.lastSuccess[run.Job] = run.StartedAt
	}
}

// classify — паника отдельно от ошибки: см. doc.go, п. 4. Panicked проверяется
// первым, потому что при панике заполнено и Err.
func classify(run scheduler.Run) Result {
	switch {
	case run.Panicked:
		return ResultPanic
	case run.Err != nil:
		return ResultError
	default:
		return ResultOK
	}
}

// unixSeconds — unix-время с дробной частью: единица гейджа s, а порог алерта
// сравнивается с time().
func unixSeconds(at time.Time) float64 {
	return float64(at.UnixNano()) / float64(time.Second)
}
