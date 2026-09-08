package outbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Status — состояние строки outbox (закрытый набор: CHECK и метка гейджа).
type Status string

const (
	StatusPending Status = "pending"
	// StatusProcessing — взята воркером, аренда до locked_until и токен в
	// claim_token; истёкшая аренда означает упавший посреди работы процесс,
	// Claim заберёт строку снова и пометит Reclaimed.
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	// StatusFailed — dead-letter: виден, хранится, чистится только Redrive.
	StatusFailed  Status = "failed"
	StatusExpired Status = "expired"
)

// AllStatuses — полный список; держит guard-тест и CHECK адаптера.
var AllStatuses = []Status{
	StatusPending, StatusProcessing, StatusDone, StatusFailed, StatusExpired,
}

// Terminal — строка больше не будет выполняться.
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusExpired:
		return true
	case StatusPending, StatusProcessing:
		return false
	}
	return false
}

// FailReason — почему строка ушла в failed (закрытый набор: метка счётчика).
type FailReason string

const (
	// FailPermanent — хендлер назвал ошибку постоянной: повтор бессмысленен.
	FailPermanent FailReason = "permanent"
	// FailExhausted — исчерпаны Config.MaxAttempts.
	FailExhausted FailReason = "exhausted"
)

// AllFailReasons — полный список; держит guard-тест.
var AllFailReasons = []FailReason{FailPermanent, FailExhausted}

// Envelope — строка outbox: сообщение плюс состояние выполнения.
type Envelope struct {
	ID            uuid.UUID
	Kind          Kind
	Payload       json.RawMessage
	DedupKey      string
	AggregateType string
	AggregateID   string
	SchemaVersion int
	Headers       map[string]string
	// Fingerprint — sha256 содержимого, 32 байта; адаптер хранит байт в байт.
	Fingerprint []byte

	Status   Status
	Attempts int
	// AvailableAt — раньше этого момента строку не забирают.
	AvailableAt time.Time
	NotAfter    *time.Time
	OccurredAt  time.Time

	// ClaimToken — токен аренды (fencing): Finish меняет строку только со
	// своим токеном. Nil во всех статусах, кроме processing.
	ClaimToken  *uuid.UUID
	LockedUntil *time.Time
	// Reclaimed — Claim взял строку из processing с истёкшей арендой: исход
	// прошлой попытки неизвестен. Транзитный флаг, в хранилище не пишется.
	Reclaimed bool

	// LastError — усечённый текст ошибки хендлера, без payload.
	LastError  string
	FailReason FailReason

	CreatedAt time.Time
	UpdatedAt time.Time
	DoneAt    *time.Time
}

// Delivery — что видит хендлер: конверт без отпечатка и токена аренды. Их
// отсутствие — не экономия: хендлеру нечего решать по чужому claim_token, а
// отпечаток он не может ни проверить, ни пересчитать.
type Delivery struct {
	ID            uuid.UUID
	Kind          Kind
	Payload       json.RawMessage
	Headers       map[string]string
	AggregateType string
	AggregateID   string
	SchemaVersion int
	Attempts      int
	// Reclaimed — прошлая попытка не досказала исход: эффект мог случиться.
	// Политику решает хендлер, у пакета её нет (doc.go, п. 2).
	Reclaimed  bool
	OccurredAt time.Time
	NotAfter   *time.Time
}

// delivery — доставка по строке. Headers копируются: хендлер не должен
// править состояние, которое адаптер отдал по ссылке.
func (e Envelope) delivery() Delivery {
	return Delivery{
		ID:            e.ID,
		Kind:          e.Kind,
		Payload:       e.Payload,
		Headers:       maps.Clone(e.Headers),
		AggregateType: e.AggregateType,
		AggregateID:   e.AggregateID,
		SchemaVersion: e.SchemaVersion,
		Attempts:      e.Attempts,
		Reclaimed:     e.Reclaimed,
		OccurredAt:    e.OccurredAt,
		NotAfter:      e.NotAfter,
	}
}

// fingerprint — сигнатура сообщения для вопроса «тот же ключ — то же
// сообщение?». sha256 с префиксом длины перед каждой секцией (иначе ("ab","c")
// и ("a","bc") склеиваются); заголовки в отсортированном порядке. Времена и
// сам DedupKey не входят: секунды между попытками не должны превращать повтор
// в конфликт.
//
// Payload сравнивается ПОБАЙТНО: пробелы и порядок ключей значимы, JSON пакет
// не нормализует. Потребитель, собирающий payload дважды разными сериализа-
// торами, получит ErrKeyReused — и это честнее тихого «уже в очереди».
func fingerprint(kind Kind, payload []byte, aggType, aggID string, schemaVersion int, headers map[string]string) []byte {
	var b bytes.Buffer
	writeLenPrefixed(&b, string(kind))
	writeLenPrefixed(&b, string(payload))
	writeLenPrefixed(&b, aggType)
	writeLenPrefixed(&b, aggID)
	writeLenPrefixed(&b, strconv.Itoa(schemaVersion))

	names := slices.Sorted(maps.Keys(headers))
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
// считается: адаптер, потерявший колонку, иначе превращал бы любое сообщение
// под тем же ключом в «уже в очереди».
func sameMessage(stored, current []byte) bool {
	return len(stored) > 0 && bytes.Equal(stored, current)
}
