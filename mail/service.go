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

// NewService строит сервис; паникует на nil store/transport и негодном Config.
// supp может быть nil — стоп-лист тогда живёт у провайдера.
//
// Ошибка конфигурации обязана падать на старте, а не на первом письме:
// первое письмо — это регистрация первого учителя.
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

// SetClock подменяет источник времени. Только для тестов и только до начала
// обслуживания: поле читается из каждого вызова, подмена под нагрузкой — гонка.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Prepare — чистая половина Enqueue: валидация, нормализация, отпечаток, id,
// Message-ID. Ни хранилища, ни времени внешнего мира здесь нет, и это
// сделано ради одного сценария: потребитель, которому нужна вставка В СВОЕЙ
// транзакции, зовёт Prepare, а строку кладёт адаптером (mailpg.Store.WithTx).
// Оба пути — Enqueue и Prepare+адаптер — проходят одну и ту же проверку,
// потому что она здесь одна.
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
		// Text обязателен, а не «Text или HTML»: часть клиентов и все
		// спам-фильтры читают текстовую часть, и письмо из одного HTML
		// доезжает хуже.
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

// Transport — имя транспорта, с которым собран сервис: метка метрики и лог.
func (s *Service) Transport() TransportName { return s.transport.Name() }
