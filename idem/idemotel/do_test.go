package idemotel_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemotel"
	"github.com/nrect/rebar/idem/idemtest"
)

var (
	errOp   = errors.New("idemotel_test: seat is taken")
	errDown = errors.New("idemotel_test: store is down")
)

// series — ряд счётчика: операция и исход.
type series struct {
	op      idem.Operation
	outcome idem.Outcome
}

// Метка совпадает с тем, что Do вернул вызывающему (PATTERNS §8): после каждого
// шага каждый ряд равен числу результатов этой операции и этого исхода,
// полученных вызывающими. Результат разбирает тест, а не хранилище: ловится и
// наблюдатель, пишущий не тот исход, и хранилище, отдающее ему не тот.
func TestObserver_LabelMatchesDoResult(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := idemotel.NewObserver(meter)
	require.NoError(t, err)
	store := idemtest.NewMemStore(testConfig(), obs)

	returned := map[series]int64{}
	do := func(ctx context.Context, req idem.Request, fn func(context.Context) (idem.Response, error)) {
		res, doErr := store.Do(ctx, req, fn)
		if outcome, counted := resultOutcome(res, doErr); counted {
			returned[series{op: req.Operation, outcome: outcome}]++
		}
	}
	ctx := t.Context()
	first, busy := request(t, opCreate, "first"), request(t, opUpdate, "busy")

	for _, step := range []struct {
		name string
		run  func()
	}{
		{"первое исполнение", func() { do(ctx, first, respond(created())) }},
		{"повтор", func() { do(ctx, first, respond(created())) }},
		{"тот же ключ, другое тело", func() {
			other := first
			other.Body = []byte(`{"product":"b"}`)
			do(ctx, other, respond(created()))
		}},
		{"тот же ключ во время op", func() {
			do(ctx, busy, func(ctx context.Context) (idem.Response, error) {
				do(ctx, busy, respond(created()))
				return created(), nil
			})
		}},
		{"ошибка op", func() { do(ctx, request(t, opCreate, "op-error"), fail(errOp)) }},
		{"ответ 5xx", func() {
			resp := created()
			resp.Status = http.StatusServiceUnavailable
			do(ctx, request(t, opUpdate, "server-error"), respond(resp))
		}},
		{"ответ сверх потолка", func() {
			resp := created()
			resp.Body = []byte(strings.Repeat("x", testConfig().MaxResponseBytes))
			do(ctx, request(t, opCreate, "too-large"), respond(resp))
		}},
		{"сбой хранилища на входе", func() {
			store.SetErr(errDown)
			defer store.SetErr(nil)
			do(ctx, request(t, opUpdate, "down"), respond(created()))
		}},
		{"сбой хранилища при фиксации", func() {
			defer store.SetErr(nil)
			do(ctx, request(t, opCreate, "commit-down"), func(context.Context) (idem.Response, error) {
				store.SetErr(errDown)
				return created(), nil
			})
		}},
		{"отменённый контекст", func() {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			do(cancelled, request(t, opUpdate, "cancelled"), respond(created()))
		}},
		{"запрос, собранный неверно", func() {
			foreign := request(t, opCreate, "invalid")
			foreign.Operation = "orders.delete"
			do(ctx, foreign, respond(created()))
			anonymous := request(t, opCreate, "invalid")
			anonymous.Scope = idem.Scope{}
			do(ctx, anonymous, respond(created()))
		}},
	} {
		step.run()
		assertSeriesMatch(t, reader, returned, step.name)
	}

	for _, outcome := range idem.AllOutcomes {
		reached := returned[series{op: opCreate, outcome: outcome}] + returned[series{op: opUpdate, outcome: outcome}]
		assert.Positive(t, reached, "исход %s ни разу не вернулся вызывающему: шаги его не покрывают", outcome)
	}
}

// resultOutcome — исход по тому, что получил вызывающий; counted = false —
// запрос отвергнут до хранилища, исхода нет. Ошибки op в тесте не несут
// sentinel idem: снаружи такую не отличить от отказа хранилища.
func resultOutcome(res idem.Result, err error) (outcome idem.Outcome, counted bool) {
	switch {
	case errors.Is(err, idem.ErrInvalidRequest), errors.Is(err, idem.ErrInvalidScope):
		return "", false
	case err == nil && res.Replayed:
		return idem.OutcomeReplayed, true
	case err == nil:
		return idem.OutcomeExecuted, true
	case errors.Is(err, idem.ErrKeyReused):
		return idem.OutcomeReused, true
	case errors.Is(err, idem.ErrInFlight):
		return idem.OutcomeInFlight, true
	case errors.Is(err, idem.ErrNotRecordable):
		return idem.OutcomeNotRecordable, true
	case errors.Is(err, idem.ErrResponseTooLarge):
		return idem.OutcomeTooLarge, true
	case errors.Is(err, idem.ErrUnavailable):
		return idem.OutcomeError, true
	default:
		return idem.OutcomeFailed, true
	}
}

// assertSeriesMatch — каждый ряд Config.Operations × AllOutcomes равен числу
// результатов, полученных вызывающими, и других рядов нет.
func assertSeriesMatch(t *testing.T, reader *sdkmetric.ManualReader, returned map[series]int64, step string) {
	t.Helper()
	points := requestPoints(t, reader)
	assert.Len(t, points, len(operations)*len(idem.AllOutcomes), "%s: рядов", step)
	for _, op := range operations {
		for _, outcome := range idem.AllOutcomes {
			value, _ := requestCount(points, op, outcome)
			assert.Equal(t, returned[series{op: op, outcome: outcome}], value,
				"%s: ряд %s/%s расходится с тем, что Do вернул вызывающим", step, op, outcome)
		}
	}
}

// request — POST /orders под ключом key в области теста.
func request(t *testing.T, op idem.Operation, key string) idem.Request {
	t.Helper()
	parsed, err := idem.ParseKey(key)
	require.NoError(t, err)
	return idem.Request{
		Scope: idem.Scope{Realm: "customers", Subject: "subject-1"}, Operation: op, Key: parsed,
		Method: http.MethodPost, Path: "/orders", Body: []byte(`{"product":"a"}`),
	}
}

func created() idem.Response {
	return idem.Response{Status: http.StatusCreated, ContentType: "application/json", Location: "/orders/1", Body: []byte(`{"order":1}`)}
}

func respond(resp idem.Response) func(context.Context) (idem.Response, error) {
	return func(context.Context) (idem.Response, error) { return resp, nil }
}

func fail(err error) func(context.Context) (idem.Response, error) {
	return func(context.Context) (idem.Response, error) { return idem.Response{}, fmt.Errorf("op: %w", err) }
}
