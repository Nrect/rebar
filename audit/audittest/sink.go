package audittest

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/nrect/rebar/audit"
)

// ErrSinkFailed — отказ двойника. Отдельная ошибка, чтобы тест не принял
// поломку стенда за штатный ErrUnavailable домена.
var ErrSinkFailed = errors.New("audittest: sink failed")

// Sink — audit.Sink в памяти. Потокобезопасен.
type Sink struct {
	mu     sync.Mutex
	events []audit.Event

	// Err — ошибка Write: с ней проверяется поведение потребителя на
	// недоступном журнале. Ставится до начала работы.
	Err error
}

var _ audit.Sink = (*Sink)(nil)

// NewSink — пустой журнал.
func NewSink() *Sink { return &Sink{} }

// Write добавляет событие в конец. Карта подробностей копируется: карта
// вызывающего живёт своей жизнью, а журнал после записи не меняется.
func (s *Sink) Write(_ context.Context, ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
	}
	s.events = append(s.events, copyEvent(ev))
	return nil
}

// Events — копии записанного в порядке записи.
func (s *Sink) Events() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]audit.Event, 0, len(s.events))
	for _, ev := range s.events {
		out = append(out, copyEvent(ev))
	}
	return out
}

// Count — сколько событий записано.
func (s *Sink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Last — последнее записанное событие; ok = false на пустом журнале.
func (s *Sink) Last() (audit.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return audit.Event{}, false
	}
	return copyEvent(s.events[len(s.events)-1]), true
}

// ByAction — события этого действия в порядке записи.
func (s *Sink) ByAction(action audit.Action) []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Event
	for _, ev := range s.events {
		if ev.Action == action {
			out = append(out, copyEvent(ev))
		}
	}
	return out
}

// copyEvent — событие с собственной картой подробностей; nil остаётся nil,
// чтобы двойник не приукрашивал то, что ему дали.
func copyEvent(ev audit.Event) audit.Event {
	if ev.Details != nil {
		ev.Details = maps.Clone(ev.Details)
	}
	return ev
}
