package inboxtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
)

// Источники и тип набора: ключ живёт в пределах источника.
const (
	suiteSource inbox.SourceName = "suite_main"
	suiteOther  inbox.SourceName = "suite_other"
	suiteType   inbox.EventType  = "order.paid"
)

// errWaited — хранилище ждало чужую транзакцию на ключе вместо in_flight.
var errWaited = errors.New("inboxtest: Accept waited on a busy key instead of answering in_flight")

// Reader — окно набора в хранилище мимо порта: у двойника — его методы, у
// адаптера — запросы теста к базе.
type Reader interface {
	// Mark — отметка события; false — её нет.
	Mark(ctx context.Context, source inbox.SourceName, id string) (Mark, bool, error)
	// Payload — тело события; false — его нет.
	Payload(ctx context.Context, source inbox.SourceName, id string) ([]byte, bool, error)
}

// storeFixture — хранилище сценария, его окно и обработчик набора.
type storeFixture struct {
	store  inbox.Store
	reader Reader
	rec    *recorder
}

// recorder — обработчик набора: считает вызовы по ключу и отдаёт решение
// behave; behave зовётся без замка, чтобы изнутри можно было звать хранилище.
type recorder struct {
	mu     sync.Mutex
	calls  map[eventKey]int
	seen   map[eventKey]inbox.Event
	behave func(ctx context.Context, ev inbox.Event) error
}

func newRecorder() *recorder {
	return &recorder{calls: map[eventKey]int{}, seen: map[eventKey]inbox.Event{}}
}

// Handle — вызов обработчика: счёт, копия события, решение behave.
func (r *recorder) Handle(ctx context.Context, ev inbox.Event) error {
	r.mu.Lock()
	key := eventKey{source: ev.Source, id: ev.ID}
	r.calls[key]++
	r.seen[key] = cloneEvent(ev)
	behave := r.behave
	r.mu.Unlock()
	if behave == nil {
		return nil
	}
	return behave(ctx, ev)
}

func (r *recorder) set(behave func(ctx context.Context, ev inbox.Event) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.behave = behave
}

func (r *recorder) count(source inbox.SourceName, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[eventKey{source: source, id: id}]
}

func (r *recorder) last(source inbox.SourceName, id string) inbox.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneEvent(r.seen[eventKey{source: source, id: id}])
}

func newStoreFixture(t *testing.T, newStore StoreFactory) storeFixture {
	t.Helper()
	rec := newRecorder()
	store, reader := newStore(t, map[inbox.SourceName]Handler{suiteSource: rec, suiteOther: rec})
	if store == nil || reader == nil {
		t.Fatalf("фабрика набора отдала nil: хранилище %v, окно %v", store, reader)
	}
	return storeFixture{store: store, reader: reader, rec: rec}
}

// suiteEvent — событие, прошедшее ядро: отпечаток по телу, момент до приёма.
func suiteEvent(source inbox.SourceName, id, payload string) inbox.Event {
	sum := sha256.Sum256([]byte(payload))
	return inbox.Event{
		Source: source, ID: id, Type: suiteType,
		OccurredAt: suiteNow.Add(-time.Minute), Payload: []byte(payload), Digest: sum[:],
	}
}

func (f storeFixture) accept(t reporter, ev inbox.Event, now time.Time) inbox.Outcome {
	t.Helper()
	outcome, err := f.store.Accept(t.Context(), ev, now)
	noErr(t, err, "доставка "+ev.ID)
	return outcome
}

func (f storeFixture) mark(t reporter, source inbox.SourceName, id string) (Mark, bool) {
	t.Helper()
	mark, ok, err := f.reader.Mark(t.Context(), source, id)
	noErr(t, err, "чтение отметки "+id)
	return mark, ok
}

func (f storeFixture) payload(t reporter, source inbox.SourceName, id string) ([]byte, bool) {
	t.Helper()
	payload, ok, err := f.reader.Payload(t.Context(), source, id)
	noErr(t, err, "чтение тела "+id)
	return payload, ok
}

// stored — отметка и тело события есть, и они — ровно ev.
func (f storeFixture) stored(t reporter, ev inbox.Event, what string) {
	t.Helper()
	mark, ok := f.mark(t, ev.Source, ev.ID)
	isTrue(t, ok, what+": отметки нет")
	isTrue(t, mark.Type == ev.Type && bytes.Equal(mark.Digest, ev.Digest), what+": тип или отпечаток отметки не те")
	payload, ok := f.payload(t, ev.Source, ev.ID)
	isTrue(t, ok && bytes.Equal(payload, ev.Payload), what+": тела нет или оно не то")
}

// absent — ни отметки, ни тела.
func (f storeFixture) absent(t reporter, source inbox.SourceName, id, what string) {
	t.Helper()
	_, marked := f.mark(t, source, id)
	_, kept := f.payload(t, source, id)
	isTrue(t, !marked && !kept, what+": отметка или тело остались")
}

// acceptWithin — Accept, который обязан ответить без ожидания; ждущее
// хранилище упирается в suiteWait.
func acceptWithin(store inbox.Store, ev inbox.Event, now time.Time) (inbox.Outcome, error) {
	type result struct {
		outcome inbox.Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := store.Accept(context.Background(), ev, now)
		done <- result{outcome: outcome, err: err}
	}()
	select {
	case r := <-done:
		return r.outcome, r.err
	case <-time.After(suiteWait):
		return "", errWaited
	}
}
