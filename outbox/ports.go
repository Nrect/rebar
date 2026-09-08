package outbox

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EnqueueOutcome — что Store сделал при вставке.
type EnqueueOutcome string

const (
	OutcomeInserted EnqueueOutcome = "inserted"
	// OutcomeDuplicate — строка с этой парой (Kind, DedupKey) уже была,
	// возвращена она. Законный ли повтор, решает CheckDuplicate по отпечатку.
	OutcomeDuplicate EnqueueOutcome = "duplicate"
)

// AllEnqueueOutcomes — полный список; держит guard-тест.
var AllEnqueueOutcomes = []EnqueueOutcome{OutcomeInserted, OutcomeDuplicate}

// EnqueueResult — исход вставки плюс строка: новая либо существующая.
type EnqueueResult struct {
	Outcome  EnqueueOutcome
	Envelope Envelope
}

// ClaimRequest — что забрать и под каким токеном аренды.
type ClaimRequest struct {
	Now   time.Time
	Lease time.Duration
	Limit int
	// Kinds — типы, которые умеет этот воркер. Пустой список — пустая
	// выборка: строку без хендлера не забирают вовсе (doc.go, п. 4).
	Kinds []Kind
	// Token — токен аренды на всю пачку; с ним же придёт Finish (fencing).
	Token uuid.UUID
}

// FinishOutcome — исход строки, который Drain сообщает Store.
type FinishOutcome string

const (
	// FinishDone — хендлер отработал.
	FinishDone FinishOutcome = "done"
	// FinishSkipped — хендлер перепроверил предикат и эффекта нет (ErrSkip);
	// для строки это тот же done, для метрики — отдельный исход.
	FinishSkipped FinishOutcome = "skipped"
	// FinishRetry — → pending, available_at из запроса.
	FinishRetry  FinishOutcome = "retry"
	FinishFailed FinishOutcome = "failed"
	// FinishExpired — наступил NotAfter; хендлер не звался.
	FinishExpired FinishOutcome = "expired"
	// FinishReleased — отмена пришла ДО старта хендлера: строка возвращается
	// в pending немедленно и без потраченной попытки.
	FinishReleased FinishOutcome = "released"
)

// AllFinishOutcomes — полный список; держит guard-тест.
var AllFinishOutcomes = []FinishOutcome{
	FinishDone, FinishSkipped, FinishRetry, FinishFailed, FinishExpired, FinishReleased,
}

// FinishRequest — что записать по итогам попытки.
type FinishRequest struct {
	ID uuid.UUID
	// Token — токен аренды, под которым строка была забрана; Finish без
	// совпадения по нему не меняет ничего и возвращает ErrClaimLost.
	Token   uuid.UUID
	Outcome FinishOutcome
	Now     time.Time
	// NextAttemptAt — только для FinishRetry.
	NextAttemptAt time.Time
	// Error — текст ошибки хендлера, усечённый до MaxErrorLen по границе руны;
	// payload в нём нет по контракту хендлера.
	Error string
	// FailReason — только для FinishFailed.
	FailReason FailReason
}

// Stats — состояние очереди для гейджей потребителя.
type Stats struct {
	// Pending — строк в pending (включая отложенные, чей срок ещё не пришёл).
	Pending int64
	// Processing — строк под арендой.
	Processing int64
	// Failed — dead-letter до Redrive: Purge его не чистит.
	Failed int64
	// Unhandled — pending со Kind, которого нет в реестре воркера: их не
	// возьмёт никто. Отдельный алерт, а не «очередь растёт».
	Unhandled int64
	// OldestDueAge — now − min(available_at) по pending, чей срок УЖЕ
	// наступил. Возраст, а не глубина: «воркер жив, но ничего не уходит»
	// глубиной не ловится. Отложенные и арендованные строки не считаются —
	// иначе гейдж горел бы от штатной задержки.
	OldestDueAge time.Duration
}

// Store — порт хранилища outbox. Только примитивы, uuid, time и типы пакета в
// сигнатурах; как адаптер попадает в транзакцию потребителя — его дело
// (outboxpg.WithTx).
//
// ВСЕ ВРЕМЕНА ПРИХОДЯТ ПАРАМЕТРОМ. Ни now(), ни DEFAULT now() в колонках,
// которые пишет домен: иначе тесты ядра на управляемых часах проверяют одно,
// а база пишет другое (CONVENTIONS §9).
type Store interface {
	// Enqueue вставляет строку в pending. Реализация обязана иметь
	// UNIQUE (kind, dedup_key) WHERE dedup_key <> '' и на конфликт ИМЕННО ПО
	// НЕМУ (ON CONFLICT DO NOTHING по этому индексу плюс SELECT, а не
	// перехват 23505) вернуть OutcomeDuplicate с существующей строкой и её
	// Fingerprint байт в байт, не роняя транзакцию вызывающего: ошибка
	// Postgres переводит транзакцию бизнес-факта в aborted, и законный повтор
	// ронял бы сам факт. Пустой DedupKey дедупу не подлежит — такие строки
	// вставляются всегда.
	Enqueue(ctx context.Context, env Envelope) (EnqueueResult, error)

	// Claim забирает до req.Limit строк с Kind из req.Kinds: pending с
	// available_at <= req.Now и processing с locked_until < req.Now (этим
	// реализация ставит Reclaimed по прежнему статусу). Порядок
	// (available_at, id); блокировка FOR UPDATE SKIP LOCKED или эквивалент.
	// Взятые строки переводятся в processing: attempts += 1,
	// claim_token = req.Token, locked_until = req.Now + req.Lease.
	//
	// Непозитивный Limit или пустой Kinds — пустая выборка БЕЗ ошибки:
	// ошибка Claim остановила бы прогон, а «мне нечего забирать» не сбой.
	Claim(ctx context.Context, req ClaimRequest) ([]Envelope, error)

	// Finish записывает исход строки ТОЛЬКО если status = 'processing' И
	// claim_token = req.Token. Ноль обновлённых строк — ErrClaimLost, и
	// состояние при этом не меняется: проснувшийся воркер A не перепишет
	// результат воркера B (doc.go, п. 3).
	//
	// Исходы: done и skipped → done, done_at = req.Now; retry → pending,
	// available_at = req.NextAttemptAt; failed → failed, fail_reason =
	// req.FailReason; expired → expired; released → pending, attempts − 1
	// (не ниже нуля), available_at = req.Now.
	//
	// PAYLOAD НЕ СТИРАЕТСЯ НИКОГДА, включая failed: без него redrive
	// невозможен (doc.go, п. 7). claim_token и locked_until обнуляются во
	// всех исходах; last_error = req.Error, updated_at = req.Now.
	Finish(ctx context.Context, req FinishRequest) error

	// Stats — снимок очереди на момент now. known — типы, которые умеет
	// воркер: pending со Kind вне этого списка попадают в Unhandled.
	Stats(ctx context.Context, now time.Time, known []Kind) (Stats, error)

	// ListFailed — dead-letter для оператора, самые старые первыми, не больше
	// limit. Непозитивный limit — пустая выборка без ошибки.
	ListFailed(ctx context.Context, limit int) ([]Envelope, error)

	// Redrive возвращает строку из failed в работу: status = pending,
	// attempts = 0, fail_reason = '', available_at = now. Только из failed;
	// false означает «строки нет либо она не в failed» и НЕ является ошибкой.
	// last_error сохраняется: оператору видно, из-за чего строка попала в
	// dead-letter.
	Redrive(ctx context.Context, id uuid.UUID, now time.Time) (bool, error)

	// Purge удаляет строки в done и expired с updated_at < before, не больше
	// limit за вызов; возвращает число удалённых. failed НЕ ТРОГАЕТ: молча
	// исчезнувший dead-letter — это потерянное событие без следов.
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}

// Handler — что делает потребитель с доставленным сообщением. Хендлер обязан
// быть идемпотентным: доставка at-least-once, и Delivery.Reclaimed говорит,
// что прошлая попытка могла оставить эффект (doc.go, п. 2).
//
// nil — done; ErrSkip — done(skipped); ошибка с Permanent() — failed без
// ретраев; ошибка с RetryAfter() — повтор не раньше названного срока; любая
// другая — временный сбой.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc — функция как Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle вызывает саму функцию.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }
