package inbox_test

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
)

const (
	billing   inbox.SourceName = "billing"
	typePaid  inbox.EventType  = "invoice.paid"
	typeDraft inbox.EventType  = "invoice.draft"
	maxBody                    = 4096
)

var (
	start  = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	secret = []byte("inbox-test-secret-0123456789abcdef")
	okAck  = inbox.Ack{ContentType: "text/plain; charset=utf-8", Body: []byte("OK")}
)

// harness — сервис на двойниках: тестовая подпись, обработчик и верификатор
// со счётом вызовов, управляемые часы.
type harness struct {
	svc      *inbox.Service
	store    *inboxtest.MemStore
	obs      *inboxtest.Observer
	clock    *inboxtest.Clock
	handler  *countingHandler
	verifier *countingVerifier
}

func newHarness(t *testing.T) harness {
	t.Helper()
	clock := inboxtest.NewClock(start)
	handler := &countingHandler{}
	verifier := &countingVerifier{next: inboxtest.NewHMACVerifier(billing, 5*time.Minute, clock.Now, secret)}
	return newHarnessWith(clock, handler, verifier)
}

func newHarnessWith(clock *inboxtest.Clock, handler *countingHandler, verifier *countingVerifier) harness {
	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{billing: handler})
	obs := inboxtest.NewObserver()
	svc := inbox.NewService(store, obs, testConfig(verifier))
	svc.SetClock(clock.Now)
	return harness{svc: svc, store: store, obs: obs, clock: clock, handler: handler, verifier: verifier}
}

func testConfig(verifier inbox.Verifier) inbox.Config {
	return inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			billing: {
				Verifier: verifier, Handle: []inbox.EventType{typePaid}, Ignore: []inbox.EventType{typeDraft},
				Ack: inbox.Ack{ContentType: okAck.ContentType, Body: bytes.Clone(okAck.Body)},
			},
		},
		MaxBodyBytes: maxBody, Retention: 30 * 24 * time.Hour, PayloadRetention: 72 * time.Hour, PurgeBatch: 500,
	}
}

func (h harness) receive(t *testing.T, req inbox.Request) (inbox.Receipt, error) {
	t.Helper()
	return h.svc.Receive(t.Context(), billing, req)
}

func signed(id string, typ inbox.EventType, data any) inbox.Request {
	return inboxtest.SignHMAC(secret, start, inboxtest.EventBody(id, typ, data))
}

// countingHandler — обработчик теста: запоминает события и отдаёт решение behave.
type countingHandler struct {
	mu     sync.Mutex
	events []inbox.Event
	behave func(ctx context.Context, ev inbox.Event) error
}

func (h *countingHandler) Handle(ctx context.Context, ev inbox.Event) error {
	h.mu.Lock()
	h.events = append(h.events, ev)
	behave := h.behave
	h.mu.Unlock()
	if behave == nil {
		return nil
	}
	return behave(ctx, ev)
}

func (h *countingHandler) set(behave func(ctx context.Context, ev inbox.Event) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.behave = behave
}

func (h *countingHandler) handled() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

// countingVerifier — верификатор со счётом вызовов: «до проверки не дошли».
type countingVerifier struct {
	next  inbox.Verifier
	calls atomic.Int32
}

func (v *countingVerifier) Verify(ctx context.Context, req inbox.Request) (inbox.Event, error) {
	v.calls.Add(1)
	return v.next.Verify(ctx, req)
}

// verifierFunc — верификатор из функции: ответ, которого честная схема не даст.
type verifierFunc func(ctx context.Context, req inbox.Request) (inbox.Event, error)

func (f verifierFunc) Verify(ctx context.Context, req inbox.Request) (inbox.Event, error) {
	return f(ctx, req)
}
