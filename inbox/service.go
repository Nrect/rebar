package inbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/nrect/rebar/kit/errs"
)

// Service — приём доставок чужих систем поверх Store.
type Service struct {
	store      Store
	obs        Observer
	sources    map[SourceName]source
	maxBody    int
	retention  time.Duration
	payloadTTL time.Duration
	purgeBatch int
	now        func() time.Time
}

// source — приём источника после проверки Config.
type source struct {
	verifier Verifier
	handle   map[EventType]bool
	ignore   map[EventType]bool
	ack      Ack
}

// Receipt — исход доставки и подтверждение для отправителя.
type Receipt struct {
	Outcome Outcome
	// Ack — ответ 200 источника; у исхода с ошибкой пуст.
	Ack Ack
}

// NewService паникует на nil-портах, негодном Config и на источниках, которые
// разошлись с обработчиками хранилища: обработчик на каждый источник обязателен.
func NewService(store Store, obs Observer, cfg Config) *Service {
	if store == nil {
		panic("inbox.NewService: store must not be nil")
	}
	if obs == nil {
		panic("inbox.NewService: observer must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("inbox.NewService: " + err.Error())
	}
	declared := slices.Sorted(maps.Keys(cfg.Sources))
	handled := slices.Sorted(slices.Values(store.Sources()))
	if !slices.Equal(declared, handled) {
		panic(fmt.Sprintf("inbox.NewService: Config.Sources %v must match the store handlers %v: every source needs exactly one handler",
			declared, handled))
	}
	sources := make(map[SourceName]source, len(cfg.Sources))
	for name, sc := range cfg.Sources {
		sources[name] = source{
			verifier: sc.Verifier,
			handle:   setOf(sc.Handle),
			ignore:   setOf(sc.Ignore),
			ack:      Ack{ContentType: sc.Ack.ContentType, Body: bytes.Clone(sc.Ack.Body)},
		}
	}
	return &Service{
		store: store, obs: obs, sources: sources,
		maxBody: cfg.MaxBodyBytes, retention: cfg.Retention, payloadTTL: cfg.PayloadRetention, purgeBatch: cfg.PurgeBatch,
		now: time.Now,
	}
}

// SetClock подменяет часы; только для тестов и до начала приёма. nil — паника
// здесь, а не разыменование в чужом стеке.
func (s *Service) SetClock(now func() time.Time) {
	if now == nil {
		panic("inbox.Service.SetClock: now must not be nil")
	}
	s.now = now
}

// Serves — источник объявлен в Config.
func (s *Service) Serves(source SourceName) bool {
	_, ok := s.sources[source]
	return ok
}

// MaxBodyBytes — потолок тела: обвязка читает не больше него плюс байт.
func (s *Service) MaxBodyBytes() int { return s.maxBody }

// Receive — приём одной доставки источника (решение 7). Ошибка — ровно у
// исходов, которым не отвечают 200: too_large, not_authentic, malformed,
// unknown_type, in_flight и error.
//
// КЛАСС ОШИБКИ РЕШАЕТ ЯДРО: класс, положенный глубже верификатором или
// обработчиком, наружу не проступает — ошибка обработчика остаётся 503 при
// любом своём классе.
func (s *Service) Receive(ctx context.Context, name SourceName, req Request) (Receipt, error) {
	src, ok := s.sources[name]
	if !ok {
		return Receipt{}, fmt.Errorf("%w: %q", ErrUnknownSource, name)
	}
	start := s.now()
	outcome, err := s.receive(ctx, name, src, req, start)
	s.obs.Received(ctx, name, outcome, s.now().Sub(start))
	if err != nil {
		return Receipt{Outcome: outcome}, err
	}
	return Receipt{Outcome: outcome, Ack: Ack{ContentType: src.ack.ContentType, Body: bytes.Clone(src.ack.Body)}}, nil
}

func (s *Service) receive(ctx context.Context, name SourceName, src source, req Request, now time.Time) (Outcome, error) {
	if len(req.Raw) > s.maxBody {
		return OutcomeTooLarge, fmt.Errorf("%w: %d bytes over the limit of %d", ErrTooLarge, len(req.Raw), s.maxBody)
	}
	// Подлинность — первой и без базы: неподтверждённый запрос не стоит
	// соединения из пула.
	raw, err := src.verifier.Verify(ctx, req)
	if err != nil {
		return verifyFailure(err)
	}
	ev, err := prepare(raw, name, now)
	if err != nil {
		return OutcomeMalformed, err
	}
	switch {
	case src.ignore[ev.Type]:
		return OutcomeIgnored, nil
	case !src.handle[ev.Type]:
		return OutcomeUnknownType, fmt.Errorf("%w: %q", ErrUnknownType, ev.Type)
	}
	outcome, err := s.store.Accept(ctx, ev, now)
	if err != nil {
		return OutcomeError, unavailable("accept", err)
	}
	switch outcome {
	case OutcomeAccepted, OutcomeDuplicate, OutcomeConflict:
		return outcome, nil
	case OutcomeInFlight:
		return outcome, ErrInFlight
	default:
		return OutcomeError, fmt.Errorf("%w: store returned outcome %q", ErrUnavailable, outcome)
	}
}

// Purge — уборка одной пачкой: тела старше PayloadRetention, отметки старше
// Retention. Форма scheduler.Job.Run; недоделанное доделает следующий прогон.
func (s *Service) Purge(ctx context.Context) (int, error) {
	now := s.now()
	deleted, err := s.store.Purge(ctx, now.Add(-s.retention), now.Add(-s.payloadTTL), s.purgeBatch)
	if err != nil {
		return 0, unavailable("purge", err)
	}
	return deleted, nil
}

// verifyFailure — исход отказа верификатора. НЕУВЕРЕННОСТЬ — В СТОРОНУ ПОВТОРА:
// «проверить не смогли» старше отказа, а ошибка без класса — недоступность.
func verifyFailure(err error) (Outcome, error) {
	switch {
	case errors.Is(err, ErrUnavailable):
		return OutcomeError, unavailable("verify", err)
	case errors.Is(err, ErrNotAuthentic):
		return OutcomeNotAuthentic, withClass(ErrNotAuthentic, err)
	case errors.Is(err, ErrMalformed):
		return OutcomeMalformed, withClass(ErrMalformed, err)
	default:
		return OutcomeError, fmt.Errorf("%w: verify: %w", ErrUnavailable, err)
	}
}

// unavailable — err снаружи в ErrUnavailable; уже завёрнутый с верным классом
// не заворачивается дважды.
func unavailable(op string, err error) error {
	if errs.KindOf(err) == errs.KindUnavailable && errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}

// withClass — err, у которого самый внешний класс — класс sentinel: иначе класс,
// положенный верификатором глубже, решал бы ответ отправителю.
func withClass(sentinel errs.KindError, err error) error {
	if errs.KindOf(err) == sentinel.Kind() {
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

func setOf(types []EventType) map[EventType]bool {
	set := make(map[EventType]bool, len(types))
	for _, typ := range types {
		set[typ] = true
	}
	return set
}
