package mail

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EnqueueOutcome — что Store сделал при вставке.
type EnqueueOutcome string

const (
	OutcomeInserted EnqueueOutcome = "inserted"
	// OutcomeDuplicate — строка с этим DedupKey уже была, возвращена она.
	// Законный ли повтор, решает домен по отпечатку.
	OutcomeDuplicate EnqueueOutcome = "duplicate"
)

// AllEnqueueOutcomes — полный список; держит guard-тест.
var AllEnqueueOutcomes = []EnqueueOutcome{OutcomeInserted, OutcomeDuplicate}

// EnqueueResult — исход вставки плюс строка: новая либо существующая.
type EnqueueResult struct {
	Outcome  EnqueueOutcome
	Envelope Envelope
}

// FinishOutcome — исход попытки доставки, который Deliver сообщает Store.
type FinishOutcome string

const (
	FinishSent       FinishOutcome = "sent"
	FinishRetry      FinishOutcome = "retry" // → pending, next_attempt_at из запроса
	FinishFailed     FinishOutcome = "failed"
	FinishExpired    FinishOutcome = "expired"
	FinishSuppressed FinishOutcome = "suppressed"
)

// AllFinishOutcomes — полный список; держит guard-тест.
var AllFinishOutcomes = []FinishOutcome{
	FinishSent, FinishRetry, FinishFailed, FinishExpired, FinishSuppressed,
}

// FinishRequest — что записать по итогам попытки.
type FinishRequest struct {
	ID      uuid.UUID
	Outcome FinishOutcome
	Now     time.Time
	// NextAttemptAt — только для FinishRetry.
	NextAttemptAt time.Time
	// Error — усечённый текст без содержимого письма.
	Error string
	// FailReason — только для FinishFailed.
	FailReason        FailReason
	Transport         TransportName
	ProviderMessageID string
}

// Stats — состояние очереди для гейджей потребителя.
type Stats struct {
	// Pending — строк в pending и sending.
	Pending int64
	// OldestPendingAge — возраст самой старой неотправленной; ловит «воркер
	// жив, но ничего не уходит» лучше, чем число строк.
	OldestPendingAge time.Duration
	// Failed — строк в failed до Purge.
	Failed int64
}

// Store — порт хранилища outbox. Только примитивы и типы пакета в сигнатурах;
// как адаптер попадает в транзакцию потребителя — его дело (mailpg.WithTx).
type Store interface {
	// Enqueue вставляет строку в pending. Реализация обязана иметь
	// UNIQUE (dedup_key) и на конфликт именно по нему (ON CONFLICT (dedup_key)
	// либо имя индекса, не любой 23505) вернуть OutcomeDuplicate с существующей
	// строкой и её Fingerprint байт в байт, не роняя транзакцию вызывающего.
	Enqueue(ctx context.Context, env Envelope) (EnqueueResult, error)

	// Claim забирает до limit строк к отправке и переводит их в sending с
	// арендой до now+lease, увеличивая Attempts. Кандидаты: pending с
	// next_attempt_at <= now и sending с locked_until < now — этим реализация
	// ставит Reclaimed; порядок (next_attempt_at, id); блокировка SKIP LOCKED
	// или эквивалент.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Envelope, error)

	// Finish записывает исход. В терминальном статусе реализация ОБЯЗАНА
	// стереть Subject, Text, HTML и Headers. Ноль обновлённых строк — не успех,
	// а ErrUnavailable.
	Finish(ctx context.Context, req FinishRequest) error

	// Stats — состояние очереди на момент now.
	Stats(ctx context.Context, now time.Time) (Stats, error)

	// Purge удаляет терминальные строки с updated_at < before, не больше limit
	// за вызов; возвращает число удалённых.
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}

// SendResult — что транспорт сообщил после приёма письма.
type SendResult struct {
	// ProviderMessageID — идентификатор письма у провайдера; пуст, если не назван.
	ProviderMessageID string
}

// Transport — порт отправки. Send получает собранный Envelope и ничего больше:
// доступа к Store нет. *RejectedError — постоянный отказ; любая другая ошибка
// — временный сбой. ctx несёт Config.SendTimeout.
type Transport interface {
	Name() TransportName
	Send(ctx context.Context, env Envelope) (SendResult, error)
}

// SuppressReason — почему адрес в стоп-листе (закрытый набор: метка метрики).
type SuppressReason string

const (
	SuppressHardBounce SuppressReason = "hard_bounce"
	SuppressComplaint  SuppressReason = "complaint"
	SuppressManual     SuppressReason = "manual"
)

// AllSuppressReasons — полный список; держит guard-тест.
var AllSuppressReasons = []SuppressReason{SuppressHardBounce, SuppressComplaint, SuppressManual}

// Suppression — запись стоп-листа; Email уже нормализован.
type Suppression struct {
	Email  string
	Reason SuppressReason
	// Source — откуда пришло (провайдер вебхука, "admin"); в метку не попадает.
	Source string
	At     time.Time
}

// Suppressor — порт стоп-листа, необязателен (nil — стоп-лист у провайдера).
// Ошибка IsSuppressed означает «не отправлять, повторить позже».
type Suppressor interface {
	IsSuppressed(ctx context.Context, email string) (Suppression, bool, error)
	Suppress(ctx context.Context, s Suppression) error
}
