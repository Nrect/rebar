package idemtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/kit/errs"
)

// Данные набора: две операции, срок сутки и маленький потолок ответа, чтобы
// граница размера проверялась короткими телами.
const (
	suiteRealm       = "suite"
	opCreate         = idem.Operation("orders.create")
	opUpdate         = idem.Operation("orders.update")
	suiteMaxResponse = 256
	jsonType         = "application/json"
	// suiteWait — потолок ожидания там, где реализация обязана ответить сразу.
	suiteWait = 10 * time.Second
)

// suiteNow — часы записи набора: настоящее время набору не нужно.
var suiteNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// errOp — отказ op в сценариях набора: его класс обязан дойти до вызывающего
// как есть, а не под idem.ErrUnavailable.
var errOp = errs.Conflict("seat-taken")

func suiteConfig() idem.Config {
	return idem.Config{
		Operations:       []idem.Operation{opCreate, opUpdate},
		Retention:        idem.MinRetention,
		MaxResponseBytes: suiteMaxResponse,
	}
}

// fixture — реализация сценария, её Config, наблюдатель, часы и своя область.
type fixture struct {
	cfg   idem.Config
	obs   *Observer
	clock *Clock
	sub   Subject
	scope idem.Scope
}

func newFixture(t *testing.T, newSubject Factory) *fixture {
	t.Helper()
	f := &fixture{cfg: suiteConfig(), obs: NewObserver(), clock: NewClock(suiteNow)}
	f.sub = newSubject(t, f.cfg, f.obs, f.clock.Now)
	if f.sub.Do == nil || f.sub.Pruner == nil {
		t.Fatal("фабрика набора вернула Subject без Do или Pruner")
	}
	f.scope = idem.Scope{Realm: suiteRealm, Subject: randomSubject(t)}
	return f
}

func randomSubject(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("субъект набора: %v", err)
	}
	return hex.EncodeToString(b)
}

func parseKey(t *testing.T, raw string) idem.Key {
	t.Helper()
	key, err := idem.ParseKey(raw)
	noErr(t, err, "ключ набора "+raw)
	return key
}

// request — POST /orders под ключом key в области сценария.
func (f *fixture) request(t *testing.T, key string) idem.Request {
	t.Helper()
	return idem.Request{
		Scope: f.scope, Operation: opCreate, Key: parseKey(t, key),
		Method: http.MethodPost, Path: "/orders", RawQuery: "source=suite", Body: []byte(`{"product":"a"}`),
	}
}

// created — ответ «заказ n создан»: все поля записи заполнены.
func created(n int) idem.Response {
	return idem.Response{
		Status: http.StatusCreated, ContentType: jsonType,
		Location: fmt.Sprintf("/orders/%d", n), Body: fmt.Appendf(nil, `{"order":%d}`, n),
	}
}

// effect — op набора: считает исполнения.
type effect struct{ calls atomic.Int64 }

func (e *effect) respond(resp idem.Response) func(context.Context) (idem.Response, error) {
	return func(context.Context) (idem.Response, error) {
		e.calls.Add(1)
		return resp, nil
	}
}

func (e *effect) fail(err error) func(context.Context) (idem.Response, error) {
	return func(context.Context) (idem.Response, error) {
		e.calls.Add(1)
		return idem.Response{}, err
	}
}

// do — Do, после которого у наблюдателя ровно один новый исход, и это want.
func (f *fixture) do(t *testing.T, req idem.Request, op func(context.Context) (idem.Response, error), want idem.Outcome,
) (idem.Result, error) {
	t.Helper()
	return f.observe(t, req.Operation, want, func() (idem.Result, error) { return f.sub.Do(t.Context(), req, op) })
}

// observe — вызов Do, после которого у наблюдателя ровно один новый исход want.
func (f *fixture) observe(t *testing.T, op idem.Operation, want idem.Outcome, call func() (idem.Result, error),
) (idem.Result, error) {
	t.Helper()
	before := len(f.obs.Outcomes())
	res, err := call()
	got := f.obs.Outcomes()
	if len(got) != before+1 {
		t.Fatalf("исходов у наблюдателя %d после Do, ожидался ровно один новый (было %d; ошибка Do: %v)", len(got), before, err)
	}
	if got[before] != (Observed{Operation: op, Outcome: want}) {
		t.Fatalf("наблюдатель получил %+v, ожидался исход %q операции %q (ошибка Do: %v)", got[before], want, op, err)
	}
	return res, err
}

// executed — первое исполнение: ответ op без пометки повтора.
func (f *fixture) executed(t *testing.T, req idem.Request, e *effect, resp idem.Response) {
	t.Helper()
	calls := e.calls.Load()
	res, err := f.do(t, req, e.respond(resp), idem.OutcomeExecuted)
	noErr(t, err, "исполнение")
	isTrue(t, !res.Replayed, "исполненный ответ помечен повтором")
	sameResponse(t, res.Response, resp, "ответ исполнения")
	equal(t, e.calls.Load(), calls+1, "исполнений op")
}

// replayed — повтор: записанный ответ байт в байт, op не зовётся.
func (f *fixture) replayed(t *testing.T, req idem.Request, want idem.Response) {
	t.Helper()
	var e effect
	res, err := f.do(t, req, e.respond(created(999)), idem.OutcomeReplayed)
	noErr(t, err, "повтор")
	isTrue(t, res.Replayed, "ответ повтора не помечен Replayed")
	sameResponse(t, res.Response, want, "ответ повтора")
	equal(t, e.calls.Load(), int64(0), "op на повторе")
}

func (f *fixture) purge(t *testing.T, before time.Time, limit int) int {
	t.Helper()
	n, err := f.sub.Pruner.Purge(t.Context(), before, limit)
	noErr(t, err, "уборка")
	return n
}

// retryAfter — срок из структурного контракта httperr, если он есть в цепочке.
func retryAfter(err error) (time.Duration, bool) {
	var source interface{ RetryAfter() (time.Duration, bool) }
	if !errors.As(err, &source) {
		return 0, false
	}
	return source.RetryAfter()
}

// sameResponse — статус, оба заголовка и тело байт в байт.
func sameResponse(t *testing.T, got, want idem.Response, what string) {
	t.Helper()
	if got.Status != want.Status || got.ContentType != want.ContentType || got.Location != want.Location ||
		!bytes.Equal(got.Body, want.Body) {
		t.Fatalf("%s: получено %d %q %q %q, ожидалось %d %q %q %q", what,
			got.Status, got.ContentType, got.Location, got.Body, want.Status, want.ContentType, want.Location, want.Body)
	}
}

// Проверки набора на голом testing: idemtest собирается у потребителя без
// тестовых зависимостей (CONVENTIONS §3). Каждая называет, что не сошлось.

func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: неожиданная ошибка: %v", what, err)
	}
}

func errIs(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: ожидалась ошибка %v, получено %v", what, want, err)
	}
}

func equal[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: получено %v, ожидалось %v", what, got, want)
	}
}

func isTrue(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatal(what)
	}
}

// countOf — сколько раз наблюдатель видел исход.
func countOf(observed []Observed, outcome idem.Outcome) int {
	return len(slices.DeleteFunc(slices.Clone(observed), func(o Observed) bool { return o.Outcome != outcome }))
}
