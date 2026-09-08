package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Recorder — журнал поверх порта Sink.
type Recorder struct {
	sink  Sink
	cfg   Config
	now   func() time.Time
	newID func() uuid.UUID
}

// NewRecorder паникует на nil-приёмнике и негодном Config: ошибка
// конфигурации обязана падать на старте, а не на первом событии.
func NewRecorder(sink Sink, cfg Config) *Recorder {
	if sink == nil {
		panic("audit.NewRecorder: sink must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("audit.NewRecorder: " + err.Error())
	}
	return &Recorder{
		sink: sink, cfg: cfg,
		now:   func() time.Time { return time.Now().UTC() },
		newID: uuid.New,
	}
}

// SetClock подменяет источник времени; только для тестов, до начала обслуживания.
func (r *Recorder) SetClock(now func() time.Time) { r.now = now }

// Prepare — чистая половина Record: закрытый набор действий, исход и род
// актора из закрытых наборов, актор из контекста, усечение враждебного ввода,
// проверка ключей подробностей.
//
// Вынесена отдельно, чтобы потребитель мог записать событие своим адаптером в
// транзакции самого действия (auditpg.Sink.WithTx) через ту же проверку.
//
// Ошибки: ErrUnknownAction, ErrInvalidEntry, ErrNoActor, ErrForbiddenDetail,
// ErrInvalidDetail.
func (r *Recorder) Prepare(ctx context.Context, e Entry) (Event, error) {
	if !r.cfg.knowsAction(e.Action) {
		return Event{}, fmt.Errorf("%w: %q", ErrUnknownAction, e.Action)
	}
	if !e.Outcome.valid() {
		return Event{}, fmt.Errorf("%w: outcome %q must be one of %v", ErrInvalidEntry, e.Outcome, AllOutcomes)
	}
	actor, ok := ActorFrom(ctx)
	if !ok {
		return Event{}, fmt.Errorf("%w: action %q", ErrNoActor, e.Action)
	}
	if !actor.Kind.valid() {
		return Event{}, fmt.Errorf("%w: actor kind %q must be one of %v", ErrInvalidEntry, actor.Kind, AllActorKinds)
	}
	details, err := r.prepareDetails(e.Details)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:      r.newID(),
		At:      r.now().UTC(),
		Action:  e.Action,
		Outcome: e.Outcome,
		Actor: Actor{
			Kind: actor.Kind,
			ID:   sanitize(actor.ID, MaxActorIDLen),
			Name: sanitize(actor.Name, MaxActorNameLen),
		},
		Target: Target{
			Type: sanitize(e.Target.Type, MaxTargetTypeLen),
			ID:   sanitize(e.Target.ID, MaxTargetIDLen),
		},
		RequestID: sanitize(e.RequestID, MaxRequestIDLen),
		IP:        sanitize(e.IP, MaxIPLen),
		Details:   details,
	}, nil
}

// Record готовит событие и отдаёт приёмнику.
//
// Ошибка записи возвращается вызывающему обёрнутой в ErrUnavailable и ничего
// за него не решает: отказать в действии или продолжить без записи — политика
// потребителя, и она разная у входа в систему и у возврата денег.
func (r *Recorder) Record(ctx context.Context, e Entry) error {
	ev, err := r.Prepare(ctx, e)
	if err != nil {
		return err
	}
	if err = r.sink.Write(ctx, ev); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, ev.Action, err)
	}
	return nil
}

// prepareDetails — ключи проверяются, значения чистятся: ключ пишет код,
// значение приходит из запроса.
func (r *Recorder) prepareDetails(in map[string]string) (map[string]string, error) {
	if len(in) > r.cfg.MaxDetails {
		return nil, fmt.Errorf("%w: %d details, max is %d", ErrInvalidDetail, len(in), r.cfg.MaxDetails)
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		if err := checkDetailKey(key); err != nil {
			return nil, err
		}
		out[key] = sanitize(value, r.cfg.MaxDetailLen)
	}
	return out, nil
}
