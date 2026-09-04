package mail

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EnqueueOutcome — что Store СДЕЛАЛ при вставке.
type EnqueueOutcome string

const (
	// OutcomeInserted — строка вставлена.
	OutcomeInserted EnqueueOutcome = "inserted"
	// OutcomeDuplicate — строка с этим DedupKey уже была; вставки не было,
	// возвращена существующая. Законный ли это повтор, решает домен по
	// отпечатку, а не адаптер.
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
	// FinishSent — принято транспортом. Строка → sent, тело стёрто.
	FinishSent FinishOutcome = "sent"
	// FinishRetry — временный сбой. Строка → pending, next_attempt_at из
	// запроса, аренда снята.
	FinishRetry FinishOutcome = "retry"
	// FinishFailed — терминальный отказ. Строка → failed, тело стёрто.
	FinishFailed FinishOutcome = "failed"
	// FinishExpired — NotAfter наступил. Строка → expired, тело стёрто.
	FinishExpired FinishOutcome = "expired"
	// FinishSuppressed — адрес в стоп-листе. Строка → suppressed, тело стёрто.
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
	// Error — усечённый текст ошибки без содержимого письма; пуст при успехе.
	Error string
	// FailReason — только для FinishFailed.
	FailReason FailReason
	// Transport и ProviderMessageID — чем и под каким id отправлено; заполнены
	// при FinishSent, могут быть заполнены при FinishFailed (провайдер
	// отказал, но назвал письмо).
	Transport         TransportName
	ProviderMessageID string
}

// Stats — состояние очереди для гейджей потребителя.
type Stats struct {
	// Pending — строк в pending и sending.
	Pending int64
	// OldestPendingAge — возраст самой старой строки в pending/sending, ноль
	// при пустой очереди. Именно он, а не Pending, ловит «воркер жив, но ничего
	// не уходит»: число строк при этом может быть небольшим, а возраст растёт.
	OldestPendingAge time.Duration
	// Failed — строк в failed, ещё не удалённых Purge. Алерт с порогом 1.
	Failed int64
}

// Store — порт хранилища outbox; реализуется адаптером (mailpg) или
// двойником (mailtest.MemStore).
//
// В сигнатурах только примитивы и типы пакета: ни pgx.Tx, ни sql.DB. Как
// адаптер вставляет строку в транзакцию потребителя — его дело (mailpg даёт
// WithTx); контракт здесь — что вставлено и что возвращено.
type Store interface {
	// Enqueue вставляет строку в статусе pending.
	//
	// Реализация ОБЯЗАНА иметь UNIQUE (dedup_key) и при его нарушении вернуть
	// OutcomeDuplicate вместе с СУЩЕСТВУЮЩЕЙ строкой, распознав нарушение по
	// имени индекса, а не по коду 23505: любое другое нарушение уникальности —
	// не повтор. Fingerprint возвращается байт в байт: по нему домен отличает
	// законный повтор от чужого письма под тем же ключом, и пустой отпечаток
	// у существующей строки — это ErrKeyReused, а не «наверное, повтор».
	Enqueue(ctx context.Context, env Envelope) (EnqueueResult, error)

	// Claim забирает пачку к отправке и переводит её в sending с арендой до
	// now+lease, увеличивая Attempts.
	//
	// Кандидаты: pending с next_attempt_at <= now И sending с locked_until <
	// now (истёкшая аренда — упавший посреди отправки процесс). Порядок —
	// (next_attempt_at, id) по возрастанию. Реализация обязана брать строки с
	// блокировкой SKIP LOCKED (или эквивалентом): два воркера на одной
	// таблице не должны получить одну строку.
	//
	// Строки с истёкшей арендой помечаются в ответе тем, что Attempts > 1 при
	// прежнем LastError — домен по этому решает Config.Uncertain.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Envelope, error)

	// Finish записывает исход попытки. В любом терминальном статусе
	// реализация ОБЯЗАНА стереть Subject, Text, HTML и Headers строки: тело —
	// секрет («Безопасность», п. 3), а хранить его после исхода незачем.
	//
	// Ноль обновлённых строк — не успех: строку могли удалить или её аренда
	// перехвачена. Реализация возвращает ErrUnavailable с причиной, домен
	// логирует; письмо при этом могло уйти, и это единственный случай, где
	// at-least-once виден снаружи.
	Finish(ctx context.Context, req FinishRequest) error

	// Stats — состояние очереди на момент now. Кормит гейджи потребителя.
	Stats(ctx context.Context, now time.Time) (Stats, error)

	// Purge удаляет терминальные строки с updated_at < before, не больше
	// limit за вызов. Возвращает число удалённых. Пачками, а не разом:
	// DELETE миллиона строк держит блокировку и раздувает WAL.
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}

// SendResult — что транспорт сообщил после приёма письма.
type SendResult struct {
	// ProviderMessageID — идентификатор письма у провайдера (SES MessageId,
	// SMTP queue id из ответа на DATA, если сервер его назвал). Пуст, если
	// транспорт ничего не назвал; по нему разбирают недоставку с провайдером.
	ProviderMessageID string
}

// Transport — порт отправки; реализуется адаптерами smtp, sesv2 и двойником
// mailtest.Transport.
//
// Send получает собранный Envelope и ничего, кроме него: доступа к Store у
// транспорта нет («Безопасность», п. 7). Возвращает *RejectedError на
// постоянный отказ; любая другая ошибка — временный сбой, и Deliver повторит
// по backoff. ctx несёт Config.SendTimeout, транспорт обязан его соблюдать:
// аренда строки рассчитана на этот срок.
type Transport interface {
	Name() TransportName
	Send(ctx context.Context, env Envelope) (SendResult, error)
}

// SuppressReason — почему адрес в стоп-листе. Закрытый набор: метка метрики.
type SuppressReason string

const (
	// SuppressHardBounce — адрес не существует (5xx на RCPT TO или отчёт о
	// недоставке от провайдера).
	SuppressHardBounce SuppressReason = "hard_bounce"
	// SuppressComplaint — жалоба на спам (FBL).
	SuppressComplaint SuppressReason = "complaint"
	// SuppressManual — добавлен человеком.
	SuppressManual SuppressReason = "manual"
)

// AllSuppressReasons — полный список; держит guard-тест.
var AllSuppressReasons = []SuppressReason{SuppressHardBounce, SuppressComplaint, SuppressManual}

// Suppression — запись стоп-листа.
type Suppression struct {
	// Email — УЖЕ нормализованный (NormalizeAddress).
	Email  string
	Reason SuppressReason
	// Source — откуда пришло: имя провайдера вебхука, "admin". Свободный текст
	// для разбора, в метку не попадает.
	Source string
	At     time.Time
}

// Suppressor — порт стоп-листа. Необязателен: NewService принимает nil, и
// тогда стоп-лист живёт у провайдера (у Postbox он есть и автоматический).
//
// Ошибка IsSuppressed означает «не отправлять»: письмо остаётся в очереди и
// повторится (ErrUnavailable). Отправить, не сумев проверить, — это
// fail-open ровно там, где цена ошибки — репутация домена.
type Suppressor interface {
	IsSuppressed(ctx context.Context, email string) (Suppression, bool, error)
	Suppress(ctx context.Context, s Suppression) error
}
