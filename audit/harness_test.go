package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/audittest"
)

// Действия тестового потребителя.
const (
	actionLogin  audit.Action = "user.login"
	actionRefund audit.Action = "order.refund"
)

// testClock — управляемые часы: тесты ядра не ходят к настоящему времени
// (CONVENTIONS §5), иначе прогон мутантов недетерминирован.
var testClock = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func testConfig() audit.Config {
	return audit.Config{
		Actions:      []audit.Action{actionLogin, actionRefund},
		MaxDetails:   8,
		MaxDetailLen: 64,
	}
}

// newRecorder — сервис на двойнике и управляемых часах.
func newRecorder(t *testing.T) (*audit.Recorder, *audittest.Sink) {
	t.Helper()
	sink := audittest.NewSink()
	rec := audit.NewRecorder(sink, testConfig())
	rec.SetClock(func() time.Time { return testClock })
	return rec, sink
}

// userCtx — контекст с актором, как его кладёт обвязка входа.
func userCtx(t *testing.T) context.Context {
	t.Helper()
	return audit.NewContext(t.Context(), audit.Actor{
		Kind: audit.ActorUser,
		ID:   "u-1",
		Name: "teacher@school.ru",
	})
}

// entry — годная запись; mods правят её под тест.
func entry(mods ...func(*audit.Entry)) audit.Entry {
	e := audit.Entry{
		Action:    actionLogin,
		Outcome:   audit.OutcomeSuccess,
		Target:    audit.Target{Type: "user", ID: "u-1"},
		RequestID: "req-1",
		IP:        "203.0.113.7",
	}
	for _, mod := range mods {
		mod(&e)
	}
	return e
}
