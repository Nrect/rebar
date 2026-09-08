package outbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Producer — подготовка конвертов. Вставки у него НЕТ: строка ложится в
// хранилище только адаптером, привязанным к транзакции бизнес-факта
// (outboxpg.WithTx). Так «двойная запись» закрыта конструктивно, а не
// дисциплиной вызывающего (doc.go, п. 1).
type Producer struct {
	cfg   Config
	now   func() time.Time
	newID func() uuid.UUID
}

// NewProducer паникует на nil store и негодном Config: ошибка проводки
// обязана падать на старте, а не на первом событии.
//
// ССЫЛКА НА STORE НЕ СОХРАНЯЕТСЯ, и это не упущение: поле store потребовало
// бы метода, который им пользуется, а любой такой метод — это и есть
// «Enqueue через пул», от которого пакет отказался. Порт в сигнатуре нужен,
// чтобы негодная проводка (nil-хранилище) падала здесь же, рядом с
// NewWorker, а не через сутки на первой вставке.
func NewProducer(store Store, cfg Config) *Producer {
	if store == nil {
		panic("outbox.NewProducer: store must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("outbox.NewProducer: " + err.Error())
	}
	return &Producer{
		cfg:   cfg,
		now:   func() time.Time { return time.Now().UTC() },
		newID: uuid.New,
	}
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
func (p *Producer) SetClock(now func() time.Time) { p.now = now }

// Prepare — чистая половина вставки: валидация, нормализация ключа, отпечаток,
// идентификатор, времена. Ни хранилища, ни сети — именно поэтому потребитель
// может позвать её до открытия транзакции, а вставить внутри.
//
// Ошибки: ErrBadKind, ErrInvalidMessage, ErrKeyInvalid.
func (p *Producer) Prepare(msg Message) (Envelope, error) {
	if !p.cfg.knowsKind(msg.Kind) {
		return Envelope{}, fmt.Errorf("%w: %q", ErrBadKind, msg.Kind)
	}
	key, err := normalizedKey(msg.DedupKey)
	if err != nil {
		return Envelope{}, err
	}
	if payloadErr := p.checkPayload(msg.Payload); payloadErr != nil {
		return Envelope{}, payloadErr
	}
	if msg.SchemaVersion < 1 {
		return Envelope{}, fmt.Errorf("%w: schema version %d must be at least 1",
			ErrInvalidMessage, msg.SchemaVersion)
	}
	if aggErr := checkAggregate(msg.AggregateType, msg.AggregateID); aggErr != nil {
		return Envelope{}, aggErr
	}
	headers, err := validateHeaders(msg.Headers)
	if err != nil {
		return Envelope{}, err
	}
	return p.envelope(msg, key, headers), nil
}

// normalizedKey — пустой ключ законен и означает «без дедупа»: событие,
// у которого нет естественного ключа, не должно выдумывать его.
func normalizedKey(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	return NormalizeKey(raw)
}

func (p *Producer) checkPayload(payload json.RawMessage) error {
	if len(payload) == 0 {
		return fmt.Errorf("%w: payload is empty", ErrInvalidMessage)
	}
	if len(payload) > p.cfg.MaxPayloadBytes {
		return fmt.Errorf("%w: payload is %d bytes, max is %d",
			ErrInvalidMessage, len(payload), p.cfg.MaxPayloadBytes)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrInvalidMessage)
	}
	return nil
}

// envelope — времена в UTC: строка переживает смену часового пояса процесса,
// и сравнение с available_at в базе не зависит от локали воркера.
func (p *Producer) envelope(msg Message, key string, headers map[string]string) Envelope {
	now := p.now().UTC()
	// Payload копируется: отпечаток считается по этим байтам, и правка среза
	// вызывающим между Prepare и вставкой расходила бы содержимое с подписью,
	// то есть тихо ломала бы «громкую идемпотентность».
	payload := bytes.Clone(msg.Payload)
	env := Envelope{
		ID:            p.newID(),
		Kind:          msg.Kind,
		Payload:       payload,
		DedupKey:      key,
		AggregateType: msg.AggregateType,
		AggregateID:   msg.AggregateID,
		SchemaVersion: msg.SchemaVersion,
		Headers:       headers,
		Fingerprint: fingerprint(msg.Kind, payload,
			msg.AggregateType, msg.AggregateID, msg.SchemaVersion, headers),
		Status:      StatusPending,
		AvailableAt: now,
		OccurredAt:  now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if !msg.OccurredAt.IsZero() {
		env.OccurredAt = msg.OccurredAt.UTC()
	}
	if !msg.NotBefore.IsZero() {
		env.AvailableAt = msg.NotBefore.UTC()
	}
	if !msg.NotAfter.IsZero() {
		notAfter := msg.NotAfter.UTC()
		env.NotAfter = &notAfter
	}
	return env
}

// CheckDuplicate — законен ли повтор. Тот же (Kind, DedupKey) с тем же
// отпечатком — успех, с другим — ErrKeyReused, а не тихий no-op: иначе
// «начислить 100» под ключом «начислить 500» молча превратилось бы в
// «уже сделано» (doc.go, п. 5).
//
// Отдельной функцией, а не внутри Enqueue, потому что Enqueue у пакета нет:
// вставку делает адаптер в транзакции потребителя, и проверка зовётся сразу
// за ней, там же.
func CheckDuplicate(env Envelope, res EnqueueResult) (EnqueueResult, error) {
	if res.Outcome == OutcomeDuplicate && !sameMessage(res.Envelope.Fingerprint, env.Fingerprint) {
		// Ни ключа, ни payload в тексте: ключ выводится из факта, payload — данные.
		return EnqueueResult{}, fmt.Errorf("%w: kind %q", ErrKeyReused, env.Kind)
	}
	return res, nil
}
