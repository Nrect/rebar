package inboxtest

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/nrect/rebar/inbox"
)

// Ошибки двойника, отличимые от доменных.
var (
	// ErrNoHandler — у источника события нет обработчика: дефект сборки стенда.
	ErrNoHandler = errors.New("inboxtest: event source has no handler in the store")
	// ErrSchemaCheck — событие мимо ядра, которого не примет схема: форма ключа,
	// типа или отпечатка, момент события позже приёма, тело за потолком. Как у
	// адаптера, сбой базы приходит в inbox.ErrUnavailable.
	ErrSchemaCheck = fmt.Errorf("%w: inboxtest: event breaks a schema check", inbox.ErrUnavailable)
)

// Handler — обработчик источника: форма inboxpg.Handler без pgx.Tx. Ошибка —
// «не решили»: ни отметки, ни тела, и повтор отправителя применится.
type Handler interface {
	Handle(ctx context.Context, ev inbox.Event) error
}

// HandlerFunc — функция как Handler.
type HandlerFunc func(ctx context.Context, ev inbox.Event) error

// Handle зовёт f.
func (f HandlerFunc) Handle(ctx context.Context, ev inbox.Event) error { return f(ctx, ev) }

// Mark — отметка события так, как её отдаёт база.
type Mark struct {
	Type       inbox.EventType
	Digest     []byte
	OccurredAt time.Time
	ReceivedAt time.Time
}

// MemStore — inbox.Store в памяти: ключ дедупа в пределах источника, отпечаток,
// CHECK схемы, тело отдельно от отметки, моменты как в timestamptz.
//
// ИСКЛЮЧЕНИЕ ПО КЛЮЧУ, А НЕ ОБЩИМ ЗАМКОМ (ADR-0012, решение 14): ключ
// помечается «в работе» под замком, обработчик зовётся без замка, исход
// пишется под ним снова. Вызов с тем же ключом во время обработчика получает
// in_flight, как у адаптера с блокировкой ключа; общий замок заставил бы его
// ждать и спрятал бы in_flight от тестов потребителя. Изнутри обработчика
// двойник звать можно.
//
// ПУБЛИЧНЫХ ПОЛЕЙ НЕТ: ручки — методы под замком (CONVENTIONS §3).
type MemStore struct {
	mu       sync.Mutex
	handlers map[inbox.SourceName]Handler
	events   map[eventKey]*stored
	inWork   map[eventKey]bool
	err      error
	calls    map[string]int
}

var _ inbox.Store = (*MemStore)(nil)

type eventKey struct {
	source inbox.SourceName
	id     string
}

// stored — строка отметки и строка тела: тело убирается раньше, как в
// inbox_payloads.
type stored struct {
	mark       Mark
	payload    []byte
	hasPayload bool
}

// NewMemStore — пустое хранилище с обработчиками источников. Паникует на
// пустой карте, негодном имени и nil-обработчике.
func NewMemStore(handlers map[inbox.SourceName]Handler) *MemStore {
	if len(handlers) == 0 {
		panic("inboxtest.NewMemStore: at least one source handler is required")
	}
	for _, name := range slices.Sorted(maps.Keys(handlers)) {
		if !name.Valid() {
			panic(fmt.Sprintf("inboxtest.NewMemStore: source %q must match [a-z0-9_]{1,%d}", name, inbox.MaxSourceLen))
		}
		if handlers[name] == nil {
			panic(fmt.Sprintf("inboxtest.NewMemStore: handler of source %q must not be nil", name))
		}
	}
	return &MemStore{
		handlers: maps.Clone(handlers),
		events:   map[eventKey]*stored{},
		inWork:   map[eventKey]bool{},
		calls:    map[string]int{},
	}
}

// SetErr — если не nil, Accept и Purge отвечают ею в inbox.ErrUnavailable, как
// сбой адаптера; выставленная во время обработчика роняет коммит. nil снимает.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// CallCount — сколько раз звали Accept или Purge: для утверждений «до
// хранилища не дошли».
func (m *MemStore) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

// Sources — источники с обработчиком, по имени.
func (m *MemStore) Sources() []inbox.SourceName {
	return slices.Sorted(maps.Keys(m.handlers))
}

// Accept — приём события (контракт inbox.Store.Accept).
func (m *MemStore) Accept(ctx context.Context, ev inbox.Event, now time.Time) (inbox.Outcome, error) {
	ev = cloneEvent(ev)
	key := eventKey{source: ev.Source, id: ev.ID}
	handler, outcome, err := m.begin(ctx, key, ev, now)
	if handler == nil {
		return outcome, err
	}
	handled := handler.Handle(ctx, cloneEvent(ev))
	return m.finish(ctx, key, ev, now, handled)
}

// begin — всё до обработчика, в порядке адаптера: карта обработчиков, первый
// поход в базу, блокировка ключа, CHECK отметки, конфликт ключа, CHECK тела.
// nil-обработчик — исход решён без него.
func (m *MemStore) begin(ctx context.Context, key eventKey, ev inbox.Event, now time.Time,
) (Handler, inbox.Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Accept"]++
	handler, ok := m.handlers[ev.Source]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrNoHandler, ev.Source)
	}
	if err := m.fail(ctx, "accept"); err != nil {
		return nil, "", err
	}
	if m.inWork[key] {
		return nil, inbox.OutcomeInFlight, nil
	}
	if err := markCheck(ev, now); err != nil {
		return nil, "", err
	}
	if prior, found := m.events[key]; found {
		if bytes.Equal(prior.mark.Digest, ev.Digest) {
			return nil, inbox.OutcomeDuplicate, nil
		}
		return nil, inbox.OutcomeConflict, nil
	}
	if len(ev.Payload) > inbox.MaxPayloadBytes {
		return nil, "", fmt.Errorf("%w: payload exceeds %d bytes", ErrSchemaCheck, inbox.MaxPayloadBytes)
	}
	m.inWork[key] = true
	return handler, "", nil
}

// finish — исход после обработчика: ошибка обработчика как есть, коммит не
// проходит по отменённому контексту и на сбое.
func (m *MemStore) finish(ctx context.Context, key eventKey, ev inbox.Event, now time.Time, handled error,
) (inbox.Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inWork, key)
	if handled != nil {
		return "", handled
	}
	if err := m.fail(ctx, "commit"); err != nil {
		return "", err
	}
	m.events[key] = &stored{
		mark: Mark{
			Type: ev.Type, Digest: ev.Digest,
			OccurredAt: dbMoment(ev.OccurredAt), ReceivedAt: dbMoment(now),
		},
		payload:    ev.Payload,
		hasPayload: true,
	}
	return inbox.OutcomeAccepted, nil
}

// Purge — уборка по сроку (контракт inbox.Store.Purge).
func (m *MemStore) Purge(ctx context.Context, eventsBefore, payloadsBefore time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Purge"]++
	// Потолок адаптер проверяет до запроса: отмена его не перебивает.
	if limit <= 0 {
		return 0, fmt.Errorf("inboxtest: purge limit must be positive, got %d", limit)
	}
	if err := m.fail(ctx, "purge"); err != nil {
		return 0, err
	}
	payloads := m.oldest(dbMoment(payloadsBefore), limit, func(s *stored) bool { return s.hasPayload })
	for _, key := range payloads {
		m.events[key].payload, m.events[key].hasPayload = nil, false
	}
	marks := m.oldest(dbMoment(eventsBefore), limit, func(*stored) bool { return true })
	for _, key := range marks {
		delete(m.events, key)
	}
	return len(payloads) + len(marks), nil
}

// Mark — отметка мимо порта, для утверждений теста; false — её нет. Копия.
func (m *MemStore) Mark(_ context.Context, source inbox.SourceName, id string) (Mark, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.events[eventKey{source: source, id: id}]
	if !ok {
		return Mark{}, false, nil
	}
	mark := e.mark
	mark.Digest = bytes.Clone(mark.Digest)
	return mark, true, nil
}

// Payload — тело мимо порта; false — тела нет: не записано или убрано. Копия.
func (m *MemStore) Payload(_ context.Context, source inbox.SourceName, id string) (payload []byte, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, found := m.events[eventKey{source: source, id: id}]
	if !found || !e.hasPayload {
		return nil, false, nil
	}
	return bytes.Clone(e.payload), true, nil
}

// fail — отменённый контекст и заданный сбой: оба в inbox.ErrUnavailable, там,
// где у адаптера поход в базу.
func (m *MemStore) fail(ctx context.Context, op string) error {
	if ctx.Err() != nil {
		return storeError(op, ctx.Err())
	}
	if m.err != nil {
		return storeError(op, m.err)
	}
	return nil
}

// oldest — ключи строк, принятых раньше before, старые первыми, не больше limit.
func (m *MemStore) oldest(before time.Time, limit int, has func(*stored) bool) []eventKey {
	var keys []eventKey
	for key, e := range m.events {
		if has(e) && e.mark.ReceivedAt.Before(before) {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b eventKey) int {
		return cmp.Or(
			m.events[a].mark.ReceivedAt.Compare(m.events[b].mark.ReceivedAt),
			cmp.Compare(a.source, b.source),
			cmp.Compare(a.id, b.id),
		)
	})
	return keys[:min(len(keys), limit)]
}

// markCheck — CHECK строки отметки.
func markCheck(ev inbox.Event, now time.Time) error {
	switch {
	case !inbox.ValidEventID(ev.ID):
		return fmt.Errorf("%w: event id", ErrSchemaCheck)
	case !ev.Type.Valid():
		return fmt.Errorf("%w: event type", ErrSchemaCheck)
	case len(ev.Digest) != inbox.DigestSize:
		return fmt.Errorf("%w: digest size", ErrSchemaCheck)
	case dbMoment(ev.OccurredAt).After(dbMoment(now)):
		return fmt.Errorf("%w: occurred_at is after received_at", ErrSchemaCheck)
	}
	return nil
}

func storeError(op string, err error) error {
	return fmt.Errorf("%w: inboxtest: %s: %w", inbox.ErrUnavailable, op, err)
}

// cloneEvent — копия до последнего среза: правка значения вызывающим или
// обработчиком до «базы» не доезжает.
func cloneEvent(ev inbox.Event) inbox.Event {
	ev.Payload = bytes.Clone(ev.Payload)
	ev.Digest = bytes.Clone(ev.Digest)
	return ev
}

// dbMoment — момент так, как его хранит timestamptz.
func dbMoment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
