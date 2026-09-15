package audittest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/nrect/rebar/audit"
)

// ErrSinkFailed — отказ двойника для SetErr. Write отдаёт его в
// audit.ErrUnavailable, как auditpg; поломку стенда от штатного сбоя отличает
// errors.Is по этой sentinel.
var ErrSinkFailed = errors.New("audittest: sink failed")

// Sink — audit.Sink в памяти. Потокобезопасен целиком, включая настройку.
type Sink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

var _ audit.Sink = (*Sink)(nil)

// NewSink — пустой журнал.
func NewSink() *Sink { return &Sink{} }

// SetErr — ошибка Write: с ней проверяется поведение потребителя на
// недоступном журнале; nil снимает. Приходит в audit.ErrUnavailable, как у
// auditpg.
func (s *Sink) SetErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Write добавляет событие в конец. Карта подробностей копируется: карта
// вызывающего живёт своей жизнью, а журнал после записи не меняется. Момент
// хранится так, как его хранит timestamptz у auditpg: в UTC и до микросекунд.
func (s *Sink) Write(_ context.Context, ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Сбой так, как его отдаёт auditpg: голая причина дала бы потребителю,
	// пишущему в транзакции действия, 500 там, где прод отвечает 503
	// (ADR-0007, «Двойники»).
	if s.err != nil {
		return fmt.Errorf("%w: audittest: write: %w", audit.ErrUnavailable, s.err)
	}
	stored := copyEvent(ev)
	stored.At = dbMoment(stored.At)
	s.events = append(s.events, stored)
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

// dbMoment — момент так, как его хранит timestamptz: UTC и микросекунды.
// Двойник, хранящий наносекунды и зону, зеленит у потребителя сравнение меток,
// которое на базе красное.
func dbMoment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
