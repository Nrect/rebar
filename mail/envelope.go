package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Status — состояние строки outbox (закрытый набор: CHECK и метка гейджа).
type Status string

const (
	StatusPending Status = "pending"
	// StatusSending — взята воркером, аренда до locked_until; истёкшая аренда
	// означает упавший посреди отправки процесс, Claim заберёт строку снова.
	StatusSending Status = "sending"
	StatusSent    Status = "sent"
	// StatusFailed — терминальный отказ, причина в FailReason.
	StatusFailed     Status = "failed"
	StatusExpired    Status = "expired"
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

// FailReason — почему строка ушла в failed (закрытый набор: метка счётчика).
type FailReason string

const (
	FailRejected  FailReason = "rejected"  // провайдер отказал определённо
	FailExhausted FailReason = "exhausted" // исчерпаны MaxAttempts
	FailUncertain FailReason = "uncertain" // аренда истекла при Config.Uncertain = Park
)

// AllFailReasons — полный список; держит guard-тест.
var AllFailReasons = []FailReason{FailRejected, FailExhausted, FailUncertain}

// TransportName — имя транспорта: метка метрики и колонка.
type TransportName string

// Envelope — строка outbox: письмо плюс состояние доставки.
type Envelope struct {
	ID   uuid.UUID
	Kind Kind
	To   Address
	// From — снапшот Config.From на момент постановки в очередь.
	From    Address
	Subject string
	Text    string
	HTML    string
	Headers map[string]string

	// DedupKey — уже нормализованный; Fingerprint — sha256 содержимого, 32
	// байта, адаптер хранит байт в байт.
	DedupKey    string
	Fingerprint []byte
	// MessageID — детерминирован от ID: при повторной отправке тот же, что даёт
	// почтовому клиенту шанс схлопнуть дубль.
	MessageID string

	Status        Status
	Attempts      int
	NextAttemptAt time.Time
	LockedUntil   *time.Time
	// LastError — усечённый текст ошибки транспорта без тела.
	LastError  string
	FailReason FailReason

	Transport         TransportName
	ProviderMessageID string

	NotAfter  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	SentAt    *time.Time
}

// fingerprint — сигнатура письма для вопроса «тот же ключ — то же письмо?».
// sha256 с префиксом длины перед каждой секцией (иначе ("ab","c") и ("a","bc")
// склеиваются); заголовки в отсортированном порядке. From и NotAfter не
// входят: они не содержимое письма.
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

// writeInt64 — канонический big-endian; запись в bytes.Buffer не может не удаться.
func writeInt64(b *bytes.Buffer, v int64) {
	_ = binary.Write(b, binary.BigEndian, v)
}

// sameMessage — законный ли повтор. Пустой сохранённый отпечаток повтором не
// считается: адаптер, потерявший колонку, иначе превращал бы любое письмо под
// тем же ключом в «уже в очереди».
func sameMessage(stored, current []byte) bool {
	return len(stored) > 0 && bytes.Equal(stored, current)
}

func messageID(id uuid.UUID, domain string) string {
	return "<" + id.String() + "@" + domain + ">"
}
