package authtest

import (
	"context"
	"sync"

	"github.com/nrect/rebar/auth/session"
)

// RecordingNotifier — двойник порта session.Notifier: копит письма, чтобы тест
// проверил, что владелец занятого адреса о попытке узнал.
//
// Наружу отдаётся копия среза: правка полученного не должна менять состояние
// двойника (docs/PATTERNS.md, паттерн 7).
type RecordingNotifier struct {
	mu    sync.Mutex
	notes []session.Notification
	err   error
}

// NewRecordingNotifier — пустой двойник.
func NewRecordingNotifier() *RecordingNotifier { return &RecordingNotifier{} }

// SetErr — отказ Notify: письмо не записывается; nil снимает.
func (n *RecordingNotifier) SetErr(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

// Notify записывает письмо.
func (n *RecordingNotifier) Notify(_ context.Context, note session.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.notes = append(n.notes, note)
	return nil
}

// Notifications — копия отправленного.
func (n *RecordingNotifier) Notifications() []session.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]session.Notification, len(n.notes))
	copy(out, n.notes)
	return out
}

// CountOf — сколько писем этого вида ушло.
func (n *RecordingNotifier) CountOf(kind session.NotificationKind) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	var c int
	for _, note := range n.notes {
		if note.Kind == kind {
			c++
		}
	}
	return c
}

// RecordingAuditor — двойник порта session.Auditor.
type RecordingAuditor struct {
	mu     sync.Mutex
	events []session.Event
	err    error
}

// NewRecordingAuditor — пустой двойник.
func NewRecordingAuditor() *RecordingAuditor { return &RecordingAuditor{} }

// SetErr — отказ Record: событие не записывается; nil снимает.
func (a *RecordingAuditor) SetErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// Record записывает событие.
func (a *RecordingAuditor) Record(_ context.Context, ev session.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, ev)
	return nil
}

// Events — копия журнала.
func (a *RecordingAuditor) Events() []session.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]session.Event, len(a.events))
	copy(out, a.events)
	return out
}

// CountOf — сколько событий этого вида записано.
func (a *RecordingAuditor) CountOf(kind session.EventKind) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var c int
	for _, ev := range a.events {
		if ev.Kind == kind {
			c++
		}
	}
	return c
}

// Порты реализованы.
var (
	_ session.Notifier = (*RecordingNotifier)(nil)
	_ session.Auditor  = (*RecordingAuditor)(nil)
)
