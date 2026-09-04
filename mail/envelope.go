package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Status — состояние строки outbox. Закрытый набор: колонка с CHECK и метка
// гейджа.
type Status string

const (
	// StatusPending — ждёт отправки (в том числе повторной, после
	// next_attempt_at).
	StatusPending Status = "pending"
	// StatusSending — взята воркером, аренда до locked_until. Строка с
	// истёкшей арендой — это упавший посреди отправки процесс, и Claim
	// забирает её снова (см. «Безопасность», п. 5).
	StatusSending Status = "sending"
	// StatusSent — транспорт подтвердил приём. Тело стёрто.
	StatusSent Status = "sent"
	// StatusFailed — терминальный отказ: RejectedError провайдера, исчерпаны
	// MaxAttempts либо неопределённый исход при Config.Uncertain = Park.
	// Причина — в LastError и в FailReason.
	StatusFailed Status = "failed"
	// StatusExpired — NotAfter наступил раньше отправки.
	StatusExpired Status = "expired"
	// StatusSuppressed — адрес в стоп-листе на момент отправки.
	StatusSuppressed Status = "suppressed"
)

// AllStatuses — полный список; держит guard-тест и CHECK адаптера.
var AllStatuses = []Status{
	StatusPending, StatusSending, StatusSent, StatusFailed, StatusExpired, StatusSuppressed,
}

// Terminal — тело письма больше не нужно и обязано быть стёрто.
func (s Status) Terminal() bool {
	switch s {
	case StatusSent, StatusFailed, StatusExpired, StatusSuppressed:
		return true
	case StatusPending, StatusSending:
		return false
	}
	return false
}

// FailReason — почему строка ушла в failed. Закрытый набор: метка счётчика
// отказов; код провайдера в неё не попадает.
type FailReason string

const (
	// FailRejected — провайдер отказал определённо (RejectedError).
	FailRejected FailReason = "rejected"
	// FailExhausted — исчерпаны MaxAttempts временных сбоев.
	FailExhausted FailReason = "exhausted"
	// FailUncertain — исход последней попытки неизвестен (аренда истекла),
	// а Config.Uncertain = UncertainPark.
	FailUncertain FailReason = "uncertain"
)

// AllFailReasons — полный список; держит guard-тест.
var AllFailReasons = []FailReason{FailRejected, FailExhausted, FailUncertain}

// TransportName — имя транспорта: метка метрики и колонка. Закрытый набор
// объявляют адаптеры; ядро знает только, что строка непуста.
type TransportName string

// Envelope — строка outbox: письмо плюс состояние доставки. Всё, что нужно
// транспорту, здесь есть; в Store ничего дополнительно не читается.
type Envelope struct {
	ID   uuid.UUID
	Kind Kind
	To   Address
	// From — снапшот отправителя на момент постановки в очередь. Смена
	// Config.From между Enqueue и Deliver не должна менять уже написанное
	// письмо: ответ на него придёт на тот адрес, что был в нём.
	From    Address
	Subject string
	Text    string
	HTML    string
	Headers map[string]string

	// DedupKey — УЖЕ нормализованный (NormalizeKey). Fingerprint — sha256
	// содержимого, 32 байта; адаптер хранит и возвращает байт в байт. Пустой
	// отпечаток законным повтором НЕ считается — см. sameMessage.
	DedupKey    string
	Fingerprint []byte
	// MessageID — RFC 5322 Message-ID, детерминирован от ID и
	// Config.MessageIDDomain. Один и тот же при повторной отправке той же
	// строки: единственное, что даёт почтовому клиенту шанс схлопнуть дубль.
	MessageID string

	Status   Status
	Attempts int
	// NextAttemptAt — раньше этого момента Claim строку не отдаёт.
	NextAttemptAt time.Time
	// LockedUntil непусто только в sending.
	LockedUntil *time.Time
	// LastError — текст последней ошибки транспорта, усечённый и без тела.
	LastError  string
	FailReason FailReason

	Transport         TransportName
	ProviderMessageID string

	NotAfter  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	SentAt    *time.Time
}

// fingerprint — каноническая сигнатура письма: отвечает на один вопрос «тот же
// ключ — то же письмо?».
//
// Кодирование то же, что у payment.startFingerprint: sha256, префикс длины
// перед каждой переменной секцией. Без префиксов ("ab","c") и ("a","bc")
// склеиваются, и письмо с другой темой выглядело бы законным повтором.
// Заголовки идут в отсортированном порядке: карта Go итерируется случайно, а
// отпечаток обязан совпасть между попытками.
//
// From в отпечаток НЕ входит: он берётся из Config, а не из письма, и смена
// адреса отправителя в конфиге не должна превращать повтор в ErrKeyReused.
// NotAfter не входит по той же причине, что и время у payment: сигнатура
// должна совпасть между попытками, разделёнными секундами.
func fingerprint(kind Kind, to Address, subject, text, html string, headers map[string]string) []byte {
	var b bytes.Buffer
	writeLenPrefixed(&b, string(kind))
	writeLenPrefixed(&b, to.Email)
	writeLenPrefixed(&b, to.Name)
	writeLenPrefixed(&b, subject)
	writeLenPrefixed(&b, text)
	writeLenPrefixed(&b, html)

	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	writeInt64(&b, int64(len(names)))
	for _, name := range names {
		writeLenPrefixed(&b, name)
		writeLenPrefixed(&b, headers[name])
	}

	sum := sha256.Sum256(b.Bytes())
	return sum[:]
}

func writeLenPrefixed(b *bytes.Buffer, s string) {
	writeInt64(b, int64(len(s)))
	b.WriteString(s)
}

// writeInt64 — канонический big-endian. Ошибку игнорируем осознанно: запись в
// bytes.Buffer не может не удаться.
func writeInt64(b *bytes.Buffer, v int64) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// sameMessage — законный ли это повтор. Пустая сохранённая сигнатура повтором
// НЕ считается: адаптер, потерявший колонку, превращал бы любое письмо под тем
// же ключом в «уже отправлено». Пусть лучше 409, чем молчание.
func sameMessage(stored, current []byte) bool {
	return len(stored) > 0 && bytes.Equal(stored, current)
}

// messageID — детерминированный Message-ID строки.
func messageID(id uuid.UUID, domain string) string {
	return "<" + id.String() + "@" + domain + ">"
}
