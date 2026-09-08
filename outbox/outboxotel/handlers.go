package outboxotel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/nrect/rebar/outbox"
)

// Result — исход доставки в метке result (ЗАКРЫТЫЙ НАБОР: на нём стоят алерты
// потребителя, и значение из внешнего мира в метку не попадает).
type Result string

const (
	ResultOK      Result = "ok"
	ResultSkipped Result = "skipped"
	ResultRetry   Result = "retry"
	// ResultThrottled — провайдер назвал срок; попытка при этом не расходуется,
	// поэтому отличать его от retry нужно и на дашборде.
	ResultThrottled Result = "throttled"
	ResultPermanent Result = "permanent"
	ResultExhausted Result = "exhausted"
	// ResultPanic — хендлер взорвался; для строки это временная ошибка.
	ResultPanic Result = "panic"
)

// AllResults — полный список; держит guard-тест.
var AllResults = []Result{
	ResultOK, ResultSkipped, ResultRetry, ResultThrottled, ResultPermanent, ResultExhausted, ResultPanic,
}

// Имена и единицы инструментов — часть контракта; Prometheus-экспортёр
// отрисует outbox_handled_total и outbox_handle_duration_seconds.
const (
	handledName  = "outbox_handled"
	durationName = "outbox_handle_duration"

	unitMessage = "{message}"
	unitSeconds = "s"
)

// Метки: оба набора закрыты — kind из Config.Kinds, result из AllResults.
const (
	attrKind   = "kind"
	attrResult = "result"
)

// spanName — имя span'а доставки; тип сообщения едет меткой, а не в имени,
// иначе имён столько же, сколько типов.
const spanName = "outbox.handle"

// Handlers — декоратор реестра: тот же outbox.Registry, но каждый хендлер
// обёрнут счётчиком, гистограммой и span'ом доставки. Ядро метрик не пишет и
// otel не импортирует (CONVENTIONS §6) — зависимость живёт здесь.
type Handlers struct {
	reg      *outbox.Registry
	tracer   trace.Tracer
	handled  metric.Int64Counter
	duration metric.Float64Histogram
	// maxAttempts — из того же outbox.Config, что уходит воркеру: без него
	// исход «попытки исчерпаны» неотличим от обычного повтора.
	maxAttempts int
}

// New паникует на nil-метре, nil-трейсере и негодном Config, как
// outbox.NewWorker: ошибка сборки обязана падать на старте. Ошибка создания
// инструмента возвращается — метрик у потребителя может не быть, а очередь
// нужна.
//
// Config берётся ЦЕЛИКОМ и тот же, что у воркера: MaxAttempts отдельным
// аргументом был бы вторым источником правды, и он разъехался бы первым.
func New(meter metric.Meter, tracer trace.Tracer, cfg outbox.Config) (*Handlers, error) {
	switch {
	case meter == nil:
		panic("outboxotel.New: nil meter")
	case tracer == nil:
		panic("outboxotel.New: nil tracer")
	case cfg.MaxAttempts <= 0:
		panic("outboxotel.New: Config.MaxAttempts must be positive")
	}
	handled, err := meter.Int64Counter(handledName,
		metric.WithUnit(unitMessage),
		metric.WithDescription("Исходы доставки сообщений очереди."),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxotel: инструмент %s: %w", handledName, err)
	}
	duration, err := meter.Float64Histogram(durationName,
		metric.WithUnit(unitSeconds),
		metric.WithDescription("Время работы хендлера: отделяет «медленно» от «падает»."),
	)
	if err != nil {
		return nil, fmt.Errorf("outboxotel: инструмент %s: %w", durationName, err)
	}
	return &Handlers{
		reg: outbox.NewRegistry(), tracer: tracer,
		handled: handled, duration: duration, maxAttempts: cfg.MaxAttempts,
	}, nil
}

// Wrap — хендлер со счётчиком, гистограммой и span'ом доставки. Нужен тому,
// кто собирает реестр сам; обычный путь — Register.
func (h *Handlers) Wrap(kind outbox.Kind, next outbox.Handler) outbox.Handler {
	if next == nil {
		panic("outboxotel.Wrap: handler for kind " + string(kind) + " must not be nil")
	}
	return &handler{owner: h, kind: kind, next: next}
}

// Register кладёт в реестр именно обёрнутый хендлер. Паника на дубле, негодном
// типе и nil-хендлере — та же, что у outbox.Registry: обёртка её не смягчает.
func (h *Handlers) Register(kind outbox.Kind, next outbox.Handler) {
	h.reg.Register(kind, h.Wrap(kind, next))
}

// Registry — реестр для outbox.NewWorker.
func (h *Handlers) Registry() *outbox.Registry { return h.reg }

type handler struct {
	owner *Handlers
	kind  outbox.Kind
	next  outbox.Handler
}

// Handle измеряет доставку и отдаёт ошибку next БЕЗ ИЗМЕНЕНИЙ: классификация
// Drain читает её структурно, по методам, и обёртка обязана это пережить.
//
// ПАНИКА СЧИТАЕТСЯ И ЛЕТИТ ДАЛЬШЕ. Результат по умолчанию — panic, поэтому
// хендлер, взорвавшийся до присваивания, попадает в счётчик; recover здесь
// нет, иначе Drain перестал бы видеть панику и не смог бы назначить строке
// временную ошибку.
func (h *handler) Handle(ctx context.Context, d outbox.Delivery) error {
	ctx, span := h.owner.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithLinks(linkTo(ctx, d)),
		trace.WithAttributes(
			attribute.String(attrKind, string(d.Kind)),
			attribute.Int("outbox.attempts", d.Attempts),
			attribute.Bool("outbox.reclaimed", d.Reclaimed),
		),
	)
	start := time.Now()
	result := ResultPanic
	defer func() {
		h.owner.observe(ctx, h.kind, result, time.Since(start))
		span.SetAttributes(attribute.String(attrResult, string(result)))
		span.End()
	}()

	err := h.next.Handle(ctx, d)
	result = h.owner.classify(d, err)
	return err
}

// linkTo — связь с породившим запросом через Headers["traceparent"]. Именно
// ссылка, а не родитель: запрос, поставивший сообщение в очередь, давно
// закончился, и делать доставку его продолжением значило бы держать его span
// открытым до конца ретраев.
func linkTo(ctx context.Context, d outbox.Delivery) trace.Link {
	if len(d.Headers) == 0 {
		return trace.Link{}
	}
	// MapCarrier только читается: Extract в него не пишет, и заголовки
	// доставки остаются такими, какими их отдал адаптер.
	producer := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier(d.Headers))
	return trace.LinkFromContext(producer)
}

// classify повторяет порядок outbox.Drain: skip раньше классов ошибок (это не
// отказ), permanent раньше throttling, throttling раньше счётчика попыток —
// названный провайдером срок лимит не жжёт.
func (h *Handlers) classify(d outbox.Delivery, err error) Result {
	switch {
	case err == nil:
		return ResultOK
	case errors.Is(err, outbox.ErrSkip):
		return ResultSkipped
	case outbox.IsPermanent(err):
		return ResultPermanent
	}
	if _, throttled := outbox.RetryAfterOf(err); throttled {
		return ResultThrottled
	}
	if d.Attempts >= h.maxAttempts { // Attempts уже увеличен Claim'ом
		return ResultExhausted
	}
	return ResultRetry
}

// observe — счётчик и гистограмма одним местом: метки собираются один раз.
//
// НИ ТЕКСТА ОШИБКИ, НИ ИДЕНТИФИКАТОРА АГРЕГАТА В МЕТКАХ: это данные, а не
// словарь, и они разносят кардинальность (ADR-0002, п. 9).
func (h *Handlers) observe(ctx context.Context, kind outbox.Kind, result Result, took time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String(attrKind, string(kind)),
		attribute.String(attrResult, string(result)),
	)
	h.handled.Add(ctx, 1, attrs)
	h.duration.Record(ctx, took.Seconds(),
		metric.WithAttributes(attribute.String(attrKind, string(kind))))
}
