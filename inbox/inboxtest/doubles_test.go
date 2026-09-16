package inboxtest_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/kit/errs"
)

var (
	errDown = errors.New("inboxtest_test: store is down")
	moment  = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
)

func nop(context.Context, inbox.Event) error { return nil }

func event(id string) inbox.Event {
	return inbox.Event{
		Source: "billing", ID: id, Type: "invoice.paid", OccurredAt: moment,
		Payload: []byte(id), Digest: make([]byte, inbox.DigestSize),
	}
}

func newStore() *inboxtest.MemStore {
	return inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{"billing": inboxtest.HandlerFunc(nop)})
}

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[inboxtest.MemStore](), reflect.TypeFor[inboxtest.Clock](),
		reflect.TypeFor[inboxtest.Observer](), reflect.TypeFor[inboxtest.HMACVerifier](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

func TestNewMemStore_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "inboxtest.NewMemStore: at least one source handler is required",
		func() { inboxtest.NewMemStore(nil) })
	assert.PanicsWithValue(t, `inboxtest.NewMemStore: source "Billing" must match [a-z0-9_]{1,32}`,
		func() {
			inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{"Billing": inboxtest.HandlerFunc(nop)})
		})
	assert.PanicsWithValue(t, `inboxtest.NewMemStore: handler of source "billing" must not be nil`,
		func() { inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{"billing": nil}) })
}

// Карта обработчиков копируется: правка вызывающим не меняет источники двойника.
func TestNewMemStore_CopiesHandlers(t *testing.T) {
	t.Parallel()

	handlers := map[inbox.SourceName]inboxtest.Handler{"billing": inboxtest.HandlerFunc(nop)}
	store := inboxtest.NewMemStore(handlers)
	handlers["delivery"] = inboxtest.HandlerFunc(nop)
	assert.Equal(t, []inbox.SourceName{"billing"}, store.Sources())
}

// Заданный сбой приходит так, как его отдаёт адаптер: класс 503,
// inbox.ErrUnavailable и причина в цепочке. Снимается nil.
func TestMemStore_SetErr(t *testing.T) {
	t.Parallel()

	store := newStore()
	store.SetErr(errDown)
	_, err := store.Accept(t.Context(), event("evt-down"), moment)
	requireUnavailable(t, err, errDown, "Accept")
	_, err = store.Purge(t.Context(), moment, moment, 10)
	requireUnavailable(t, err, errDown, "Purge")

	store.SetErr(nil)
	outcome, err := store.Accept(t.Context(), event("evt-down"), moment)
	require.NoError(t, err, "nil снимает сбой")
	assert.Equal(t, inbox.OutcomeAccepted, outcome)
}

// Сбой, выставленный во время обработчика, роняет коммит: у адаптера транзакция
// на порванном соединении не фиксируется.
func TestMemStore_SetErrDuringHandlerFailsCommit(t *testing.T) {
	t.Parallel()

	var store *inboxtest.MemStore
	store = inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		"billing": inboxtest.HandlerFunc(func(context.Context, inbox.Event) error {
			store.SetErr(errDown)
			return nil
		}),
	})
	_, err := store.Accept(t.Context(), event("evt-commit"), moment)
	requireUnavailable(t, err, errDown, "коммит")
	_, marked, readErr := store.Mark(t.Context(), "billing", "evt-commit")
	require.NoError(t, readErr)
	assert.False(t, marked, "отметка после упавшего коммита")
}

// Ошибка обработчика — как есть, без обёртки: класс решает ядро (ADR-0007,
// «Двойники»: порт без адаптера в модуле — причина голой).
func TestMemStore_HandlerErrorIsBare(t *testing.T) {
	t.Parallel()

	refused := errs.Conflict("seat-taken")
	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		"billing": inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return refused }),
	})
	_, err := store.Accept(t.Context(), event("evt-bare"), moment)
	require.ErrorIs(t, err, refused)
	assert.NotErrorIs(t, err, inbox.ErrUnavailable)
}

// Потолок уборки двойник проверяет до отмены и до сбоя, как адаптер до запроса.
func TestMemStore_PurgeLimitBeforeCancel(t *testing.T) {
	t.Parallel()

	store := newStore()
	store.SetErr(errDown)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.Purge(ctx, moment, moment, 0)
	require.Error(t, err)
	require.NotErrorIs(t, err, inbox.ErrUnavailable)
	require.NotErrorIs(t, err, context.Canceled)
}

// Счётчик вызовов — основа утверждений «до хранилища не дошли».
func TestMemStore_CallCount(t *testing.T) {
	t.Parallel()

	store := newStore()
	_, err := store.Accept(t.Context(), event("evt-count"), moment)
	require.NoError(t, err)
	_, err = store.Accept(t.Context(), event("evt-count"), moment)
	require.NoError(t, err)
	_, err = store.Purge(t.Context(), moment, moment, 10)
	require.NoError(t, err)

	assert.Equal(t, 2, store.CallCount("Accept"))
	assert.Equal(t, 1, store.CallCount("Purge"))
	assert.Zero(t, store.CallCount("Sources"))
}

// Ручки правятся на ходу: тест потребителя включает сбой, пока ручка его
// HTTP-сервера в другой горутине принимает события. Под -race это чисто.
func TestMemStore_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	store := newStore()
	observer := inboxtest.NewObserver()
	clock := inboxtest.NewClock(moment)
	whileServing(
		func() {
			for i := range 300 {
				ev := event("evt-serve-" + string(rune('a'+i%26)))
				outcome, _ := store.Accept(context.Background(), ev, clock.Now())
				observer.Watch("billing")
				observer.Received(context.Background(), "billing", outcome, time.Millisecond)
				_, _ = store.Purge(context.Background(), clock.Now(), clock.Now(), 5)
			}
		},
		func(i int) {
			if i%2 == 0 {
				store.SetErr(errDown)
				return
			}
			store.SetErr(nil)
		},
		func(int) { _ = store.CallCount("Accept") },
		func(int) { _ = observer.Deliveries() },
		func(int) { _ = observer.Watched() },
		func(int) { clock.Advance(time.Microsecond) },
	)
}

func TestObserver_ReadsAreCopies(t *testing.T) {
	t.Parallel()

	observer := inboxtest.NewObserver()
	observer.Watch("billing")
	observer.Received(t.Context(), "billing", inbox.OutcomeAccepted, time.Second)
	got := observer.Deliveries()
	got[0].Outcome = inbox.OutcomeError
	watched := observer.Watched()
	watched[0] = "delivery"
	assert.Equal(t, []inboxtest.Delivery{{Source: "billing", Outcome: inbox.OutcomeAccepted, Took: time.Second}},
		observer.Deliveries())
	assert.Equal(t, []inbox.SourceName{"billing"}, observer.Watched())
}

func TestClock(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 16, 12, 0, 0, 5, time.FixedZone("UTC+3", 3*60*60))
	clock := inboxtest.NewClock(at)
	assert.Equal(t, at, clock.Now(), "часы отдают момент как поставили: нормализует хранилище")
	clock.Advance(time.Minute)
	assert.Equal(t, at.Add(time.Minute), clock.Now())
	clock.Set(at)
	assert.Equal(t, at, clock.Now())
}

func TestNewHMACVerifier_Panics(t *testing.T) {
	t.Parallel()

	now := func() time.Time { return moment }
	secret := []byte("secret")
	assert.PanicsWithValue(t, `inboxtest.NewHMACVerifier: source "" must match [a-z0-9_]{1,32}`,
		func() { inboxtest.NewHMACVerifier("", time.Minute, now, secret) })
	assert.PanicsWithValue(t, "inboxtest.NewHMACVerifier: tolerance must be positive",
		func() { inboxtest.NewHMACVerifier("billing", 0, now, secret) })
	assert.PanicsWithValue(t, "inboxtest.NewHMACVerifier: now must not be nil",
		func() { inboxtest.NewHMACVerifier("billing", time.Minute, nil, secret) })
	assert.PanicsWithValue(t, "inboxtest.NewHMACVerifier: at least one secret is required",
		func() { inboxtest.NewHMACVerifier("billing", time.Minute, now) })
	assert.PanicsWithValue(t, "inboxtest.NewHMACVerifier: secret 1 must not be empty",
		func() { inboxtest.NewHMACVerifier("billing", time.Minute, now, secret, nil) })
}

// Тестовая схема целиком: подписанное тело даёт событие, неподписанное — отказ,
// подписанный мусор — ErrMalformed; секрет копируется при сборке.
func TestHMACVerifier(t *testing.T) {
	t.Parallel()

	secret := []byte("inboxtest-secret")
	verifier := inboxtest.NewHMACVerifier("billing", time.Minute, func() time.Time { return moment }, secret)
	secret[0] ^= 0xff

	body := inboxtest.EventBody("evt_1", "invoice.paid", map[string]int{"invoice": 1})
	signingSecret := []byte("inboxtest-secret")
	ev, err := verifier.Verify(t.Context(), inboxtest.SignHMAC(signingSecret, moment.Add(-30*time.Second), body))
	require.NoError(t, err)
	assert.Equal(t, inbox.Event{
		Source: "billing", ID: "evt_1", Type: "invoice.paid",
		OccurredAt: moment.Add(-30 * time.Second), Payload: body,
	}, ev)

	_, err = verifier.Verify(t.Context(), inboxtest.SignHMAC([]byte("other"), moment, body))
	require.ErrorIs(t, err, inbox.ErrNotAuthentic)

	for _, junk := range [][]byte{[]byte(`{"type":"invoice.paid"}`), []byte(`{"id":1,"type":"x"}`), []byte("not json")} {
		_, err = verifier.Verify(t.Context(), inboxtest.SignHMAC(signingSecret, moment, junk))
		require.ErrorIs(t, err, inbox.ErrMalformed, "подписанный мусор %q", junk)
		assert.NotErrorIs(t, err, inbox.ErrNotAuthentic)
	}
}

func TestEventBody(t *testing.T) {
	t.Parallel()

	assert.JSONEq(t, `{"id":"evt_1","type":"invoice.paid","data":{"invoice":1}}`,
		string(inboxtest.EventBody("evt_1", "invoice.paid", map[string]int{"invoice": 1})))
	assert.JSONEq(t, `{"id":"evt_2","type":"invoice.draft"}`, string(inboxtest.EventBody("evt_2", "invoice.draft", nil)))
	assert.Panics(t, func() { inboxtest.EventBody("evt_3", "x", func() {}) })
}

func requireUnavailable(t *testing.T, err, cause error, what string) {
	t.Helper()
	require.ErrorIs(t, err, inbox.ErrUnavailable, what)
	require.ErrorIs(t, err, cause, what)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), what)
}

// whileServing крутит каждую ручку в своей горутине, пока serve не отработает:
// ручка, пишущая мимо замка, не делит с serve ни одной точки синхронизации, и
// -race видит гонку при любом порядке.
func whileServing(serve func(), knobs ...func(i int)) {
	served := make(chan struct{})
	go func() {
		defer close(served)
		serve()
	}()
	var wg sync.WaitGroup
	for _, knob := range knobs {
		wg.Go(func() {
			for i := 0; ; i++ {
				knob(i)
				select {
				case <-served:
					return
				default:
				}
			}
		})
	}
	wg.Wait()
}
