package mail

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service — outbox поверх портов Store, Transport и (необязательно) Suppressor.
type Service struct {
	store     Store
	transport Transport
	supp      Suppressor
	cfg       Config
	now       func() time.Time
	newID     func() uuid.UUID
}

// NewService паникует на nil store/transport и негодном Config: ошибка
// конфигурации обязана падать на старте, а не на первом письме. supp может
// быть nil.
func NewService(store Store, transport Transport, supp Suppressor, cfg Config) *Service {
	switch {
	case store == nil:
		panic("mail.NewService: nil store")
	case transport == nil:
		panic("mail.NewService: nil transport")
	case transport.Name() == "":
		panic("mail.NewService: transport has an empty name")
	}
	if err := cfg.validate(); err != nil {
		panic("mail.NewService: " + err.Error())
	}
	from, _ := NormalizeAddress(cfg.From.Email)
	cfg.From.Email = from
	return &Service{
		store: store, transport: transport, supp: supp, cfg: cfg,
		now:   func() time.Time { return time.Now().UTC() },
		newID: uuid.New,
	}
}

// SetClock подменяет источник времени; только для тестов, до начала обслуживания.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Prepare — чистая половина Enqueue: валидация, нормализация, отпечаток, id.
// Вынесена отдельно, чтобы потребитель мог вставить строку в свою транзакцию
// адаптером (mailpg.Store.WithTx) через ту же проверку.
//
// Ошибки: ErrBadKind, ErrInvalidMessage, ErrKeyInvalid.
func (s *Service) Prepare(msg Message) (Envelope, error) {
	if !s.cfg.knowsKind(msg.Kind) {
		return Envelope{}, fmt.Errorf("%w: %q", ErrBadKind, msg.Kind)
	}
	key, err := NormalizeKey(msg.DedupKey)
	if err != nil {
		return Envelope{}, err
	}
	to, err := NormalizeAddress(msg.To.Email)
	if err != nil {
		return Envelope{}, fmt.Errorf("recipient: %w", err)
	}
	if err = checkLine(msg.To.Name); err != nil {
		return Envelope{}, fmt.Errorf("%w: recipient name: %w", ErrInvalidMessage, err)
	}
	switch {
	case msg.Subject == "":
		return Envelope{}, fmt.Errorf("%w: subject is empty", ErrInvalidMessage)
	case msg.Text == "":
		// Text обязателен: спам-фильтры читают текстовую часть, письмо из
		// одного HTML доезжает хуже.
		return Envelope{}, fmt.Errorf("%w: text body is empty", ErrInvalidMessage)
	case len(msg.Text)+len(msg.HTML) > s.cfg.MaxBodyBytes:
		return Envelope{}, fmt.Errorf("%w: body is %d bytes, max is %d",
			ErrInvalidMessage, len(msg.Text)+len(msg.HTML), s.cfg.MaxBodyBytes)
	}
	if err = checkLine(msg.Subject); err != nil {
		return Envelope{}, fmt.Errorf("%w: subject: %w", ErrInvalidMessage, err)
	}
	headers, err := validateHeaders(msg.Headers)
	if err != nil {
		return Envelope{}, err
	}

	now := s.now()
	toAddr := Address{Email: to, Name: msg.To.Name}
	id := s.newID()
	env := Envelope{
		ID:            id,
		Kind:          msg.Kind,
		To:            toAddr,
		From:          s.cfg.From,
		Subject:       msg.Subject,
		Text:          msg.Text,
		HTML:          msg.HTML,
		Headers:       headers,
		DedupKey:      key,
		Fingerprint:   fingerprint(msg.Kind, toAddr, msg.Subject, msg.Text, msg.HTML, headers),
		MessageID:     messageID(id, s.cfg.MessageIDDomain),
		Status:        StatusPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if !msg.NotAfter.IsZero() {
		notAfter := msg.NotAfter.UTC()
		env.NotAfter = &notAfter
	}
	return env, nil
}

// Transport — имя транспорта, с которым собран сервис.
func (s *Service) Transport() TransportName { return s.transport.Name() }
