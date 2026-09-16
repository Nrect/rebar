package idemtest_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemtest"
)

var errDown = errors.New("idemtest_test: store is down")

func config() idem.Config {
	return idem.Config{Operations: []idem.Operation{"orders.create"}, Retention: idem.MinRetention, MaxResponseBytes: 1024}
}

func request(t *testing.T) idem.Request {
	t.Helper()
	k, err := idem.ParseKey("k")
	require.NoError(t, err)
	return idem.Request{
		Scope:     idem.Scope{Realm: "customers", Subject: "subject-1"},
		Operation: "orders.create", Key: k, Method: http.MethodPost, Path: "/orders", Body: []byte(`{}`),
	}
}

func ok(calls *int) func(context.Context) (idem.Response, error) {
	return func(context.Context) (idem.Response, error) {
		*calls++
		return idem.Response{Status: http.StatusCreated, ContentType: "application/json", Body: []byte(`{"order":1}`)}, nil
	}
}

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[idemtest.MemStore](), reflect.TypeFor[idemtest.Observer](), reflect.TypeFor[idemtest.Clock](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

func TestMemStore_Panics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "idemtest.NewMemStore: observer must not be nil", func() { idemtest.NewMemStore(config(), nil) })
	assert.PanicsWithValue(t, "idemtest.NewMemStore: Config.Retention must be at least 24h0m0s, got 0s", func() {
		cfg := config()
		cfg.Retention = 0
		idemtest.NewMemStore(cfg, idemtest.NewObserver())
	})
	store := idemtest.NewMemStore(config(), idemtest.NewObserver())
	assert.PanicsWithValue(t, "idemtest.MemStore.SetClock: now must not be nil", func() { store.SetClock(nil) })
	assert.PanicsWithValue(t, "idemtest.MemStore.Do: op must not be nil", func() {
		_, _ = store.Do(t.Context(), request(t), nil)
	})
}

// Заданный сбой приходит так, как его отдаёт адаптер: класс 503,
// idem.ErrUnavailable и причина в одной цепочке. На входе op не зовётся, при
// фиксации запись не ложится. Снимается nil.
func TestMemStore_SetErr(t *testing.T) {
	t.Parallel()

	obs := idemtest.NewObserver()
	store := idemtest.NewMemStore(config(), obs)
	calls := 0

	store.SetErr(errDown)
	_, err := store.Do(t.Context(), request(t), ok(&calls))
	requireUnavailable(t, err, "Do")
	assert.Zero(t, calls, "op при сбое на входе")
	_, err = store.Purge(t.Context(), time.Now(), 10)
	requireUnavailable(t, err, "Purge")

	store.SetErr(nil)
	_, err = store.Do(t.Context(), request(t), func(ctx context.Context) (idem.Response, error) {
		store.SetErr(errDown)
		return ok(&calls)(ctx)
	})
	requireUnavailable(t, err, "фиксация")

	store.SetErr(nil)
	res, err := store.Do(t.Context(), request(t), ok(&calls))
	require.NoError(t, err)
	assert.False(t, res.Replayed, "после сбоя фиксации записи нет")
	assert.Equal(t, 2, calls)
	assert.Equal(t, []idemtest.Observed{
		{Operation: "orders.create", Outcome: idem.OutcomeError},
		{Operation: "orders.create", Outcome: idem.OutcomeError},
		{Operation: "orders.create", Outcome: idem.OutcomeExecuted},
	}, obs.Outcomes())
}

func requireUnavailable(t *testing.T, err error, what string) {
	t.Helper()
	require.ErrorIs(t, err, idem.ErrUnavailable, what)
	require.ErrorIs(t, err, errDown, "%s: причина в цепочке", what)
}

// Непозитивный limit адаптер отвечает до базы: ни сбой, ни отмена его не
// перебивают.
func TestMemStore_PurgeNonPositiveLimitSkipsStore(t *testing.T) {
	t.Parallel()

	store := idemtest.NewMemStore(config(), idemtest.NewObserver())
	store.SetErr(errDown)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, limit := range []int{0, -5} {
		n, err := store.Purge(ctx, time.Now(), limit)
		require.NoError(t, err, "limit %d", limit)
		assert.Zero(t, n)
	}
}

// Ручки крутятся каждая в своей горутине, пока «сервер» зовёт Do и Purge: у
// записи мимо замка нет точки синхронизации с читателем, и -race её видит.
func TestMemStore_KnobsAreRaceFree(t *testing.T) {
	t.Parallel()

	store := idemtest.NewMemStore(config(), idemtest.NewObserver())
	clock := idemtest.NewClock(time.Now())
	stop := make(chan struct{})
	var knobs sync.WaitGroup
	for _, knob := range []func(){
		func() { store.SetErr(nil) },
		func() { store.SetClock(clock.Now) },
		func() { clock.Advance(time.Microsecond) },
	} {
		knobs.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					knob()
				}
			}
		})
	}

	requests := make([]idem.Request, 200)
	for i := range requests {
		requests[i] = request(t)
		requests[i].Body = []byte{byte(i)}
	}
	var server sync.WaitGroup
	server.Go(func() {
		calls := 0
		for _, req := range requests {
			_, _ = store.Do(context.Background(), req, ok(&calls))
			_, _ = store.Purge(context.Background(), clock.Now(), 10)
		}
	})
	server.Wait()
	close(stop)
	knobs.Wait()
}

// Наблюдатель отдаёт копии: правка полученного среза его память не меняет.
func TestObserver_ReturnsCopies(t *testing.T) {
	t.Parallel()

	obs := idemtest.NewObserver()
	obs.Watch("orders.create")
	obs.Outcome(t.Context(), "orders.create", idem.OutcomeExecuted)
	outcomes := obs.Outcomes()
	outcomes[0].Outcome = idem.OutcomeError
	watched := obs.Watched()
	watched[0] = "orders.mutated"
	assert.Equal(t, idem.OutcomeExecuted, obs.Outcomes()[0].Outcome)
	assert.Equal(t, []idem.Operation{"orders.create"}, obs.Watched())
}

func TestClock(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := idemtest.NewClock(at)
	clock.Advance(time.Hour)
	assert.Equal(t, at.Add(time.Hour), clock.Now())
	clock.Set(at)
	assert.Equal(t, at, clock.Now())
}
