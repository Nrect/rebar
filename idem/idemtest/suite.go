package idemtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/kit/errs"
)

// DoFunc — Do в форме двойника: op без транзакции. Фабрика idempg заворачивает
// op в свою функцию с pgx.Tx.
type DoFunc func(ctx context.Context, req idem.Request, op func(context.Context) (idem.Response, error)) (idem.Result, error)

// Subject — реализация под набором: Do в пуле, не в транзакции потребителя, и
// уборка.
type Subject struct {
	Do     DoFunc
	Pruner idem.Pruner
}

// Factory — ПУСТОЕ хранилище, своё на сценарий, с Config и наблюдателем
// набора; now — источник момента записи (SetClock). Каждый Do — своя
// транзакция из пула: иначе параллельные сценарии не параллельны.
type Factory func(t *testing.T, cfg idem.Config, obs idem.Observer, now func() time.Time) Subject

// RunDoSuite — контрактный набор Do и idem.Pruner.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ: двойник и idempg не вправе разойтись, иначе
// тесты потребителя зелены на двойнике при сломанном проде (ADR-0012, решение
// 14). Атомарность с эффектом в базе, гонка за строку и прерванная транзакция
// потребителя — тесты idempg: набору база не видна.
func RunDoSuite(t *testing.T, newSubject Factory) {
	t.Helper()
	if newSubject == nil {
		panic("idemtest.RunDoSuite: newSubject must not be nil")
	}
	for _, sc := range doScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newFixture(t, newSubject))
		})
	}
}

type doScenario struct {
	name string
	run  func(t *testing.T, f *fixture)
}

var doScenarios = []doScenario{
	{name: "первое исполнение записывает ответ и отдаёт его без пометки повтора", run: suiteFirstRun},
	{name: "повтор после коммита — записанный ответ байт в байт, op не зовётся", run: suiteReplay},
	{name: "тот же ключ, другой запрос — ErrKeyReused без Retry-After, запись цела", run: suiteKeyReused},
	{name: "во время op тот же ключ — in_flight с Retry-After, другой ключ исполняется", run: suiteInFlight},
	{name: "параллельные вызовы одного ключа — op исполнена ровно один раз", run: suiteRace},
	{name: "ошибка op — отдаётся как есть, записи нет, следующий вызов исполняет", run: suiteOpError},
	{name: "ответ 5xx — ErrNotRecordable, записи нет", run: suiteNotRecordable},
	{name: "ответ сверх потолка — ErrResponseTooLarge, ровно потолок записывается", run: suiteTooLarge},
	{name: "разные области с одним ключом независимы", run: suiteScopes},
	{name: "уборка удаляет только старше границы и не больше limit", run: suitePurge},
	{name: "срок: запись живёт до уборки, Purger убирает старше Retention", run: suiteRetention},
	{name: "моменты — как timestamptz: граница внутри микросекунды не удаляет", run: suiteMoments},
	{name: "отменённый контекст — ErrUnavailable с причиной, записи нет", run: suiteCancelled},
	{name: "ответ записывается и отдаётся копией", run: suiteCopies},
	{name: "паника op не оставляет ключ занятым", run: suitePanic},
	{name: "запрос, собранный неверно, отвергается до хранилища и наблюдателя", run: suiteInvalidRequests},
	{name: "Config копируется при сборке", run: suiteConfigCopied},
}

func suiteFirstRun(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	f.executed(t, f.request(t, "first"), &e, created(1))
}

func suiteReplay(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "replay")
	f.executed(t, req, &e, created(1))
	f.replayed(t, req, created(1))

	quoted := req
	quoted.Key = parseKey(t, `"replay"`)
	f.replayed(t, quoted, created(1))
	equal(t, e.calls.Load(), int64(1), "исполнений op после повторов")
}

func suiteKeyReused(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "reused")
	f.executed(t, req, &e, created(1))

	for _, tc := range []struct {
		what   string
		change func(r *idem.Request)
	}{
		{"другое тело", func(r *idem.Request) { r.Body = []byte(`{"product":"b"}`) }},
		{"другой путь", func(r *idem.Request) { r.Path = "/orders/1" }},
		{"другая строка запроса", func(r *idem.Request) { r.RawQuery = "source=other" }},
		{"другой метод", func(r *idem.Request) { r.Method = http.MethodPatch }},
		{"другая операция", func(r *idem.Request) { r.Operation = opUpdate }},
	} {
		other := req
		tc.change(&other)
		_, err := f.do(t, other, e.respond(created(2)), idem.OutcomeReused)
		errIs(t, err, idem.ErrKeyReused, tc.what)
		isTrue(t, !errors.Is(err, idem.ErrInFlight), tc.what+": переиспользование выдано за in_flight")
		_, has := retryAfter(err)
		isTrue(t, !has, tc.what+": у переиспользованного ключа Retry-After")
	}
	equal(t, e.calls.Load(), int64(1), "op на переиспользованном ключе")
	f.replayed(t, req, created(1))
}

func suiteOpError(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "op-error")
	_, err := f.do(t, req, e.fail(errOp), idem.OutcomeFailed)
	errIs(t, err, errOp, "ошибка op")
	equal(t, errs.KindOf(err), errs.KindConflict, "класс ошибки op")
	isTrue(t, !errors.Is(err, idem.ErrUnavailable), "ошибка op завёрнута в ErrUnavailable")

	f.executed(t, req, &e, created(1))
	f.replayed(t, req, created(1))
}

func suiteNotRecordable(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "server-error")
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		failed := created(1)
		failed.Status = status
		_, err := f.do(t, req, e.respond(failed), idem.OutcomeNotRecordable)
		errIs(t, err, idem.ErrNotRecordable, fmt.Sprintf("ответ %d", status))
	}
	f.executed(t, req, &e, created(1))
	f.replayed(t, req, created(1))
}

func suiteTooLarge(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "too-large")
	sized := func(body int) idem.Response {
		return idem.Response{
			Status: http.StatusOK, ContentType: jsonType, Location: "/orders/1",
			Body: []byte(strings.Repeat("x", body-len(jsonType)-len("/orders/1"))),
		}
	}
	_, err := f.do(t, req, e.respond(sized(suiteMaxResponse+1)), idem.OutcomeTooLarge)
	errIs(t, err, idem.ErrResponseTooLarge, "ответ на байт больше потолка")

	f.executed(t, req, &e, sized(suiteMaxResponse))
	f.replayed(t, req, sized(suiteMaxResponse))
}

func suiteScopes(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	base := f.request(t, "shared-key")
	otherSubject, otherRealm := base, base
	otherSubject.Scope.Subject = randomSubject(t)
	otherRealm.Scope.Realm = "staff"

	f.executed(t, base, &e, created(1))
	f.executed(t, otherSubject, &e, created(2))
	f.executed(t, otherRealm, &e, created(3))
	f.replayed(t, base, created(1))
	f.replayed(t, otherSubject, created(2))
	f.replayed(t, otherRealm, created(3))
}

func suiteCancelled(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "cancelled")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.observe(t, req.Operation, idem.OutcomeError, func() (idem.Result, error) {
		return f.sub.Do(ctx, req, e.respond(created(1)))
	})
	cancelledIs(t, err, "Do по отменённому контексту")
	equal(t, e.calls.Load(), int64(0), "op по отменённому контексту")
	_, err = f.sub.Pruner.Purge(ctx, suiteNow, 10)
	cancelledIs(t, err, "Purge по отменённому контексту")

	late, cancelLate := context.WithCancel(t.Context())
	defer cancelLate()
	_, err = f.observe(t, req.Operation, idem.OutcomeError, func() (idem.Result, error) {
		return f.sub.Do(late, req, func(context.Context) (idem.Response, error) {
			e.calls.Add(1)
			cancelLate()
			return created(1), nil
		})
	})
	cancelledIs(t, err, "фиксация по отменённому контексту")

	f.executed(t, req, &e, created(2))
	f.replayed(t, req, created(2))
}

func cancelledIs(t *testing.T, err error, what string) {
	t.Helper()
	errIs(t, err, idem.ErrUnavailable, what)
	errIs(t, err, context.Canceled, what)
}

func suiteCopies(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "copies")
	resp := created(1)
	res, err := f.do(t, req, e.respond(resp), idem.OutcomeExecuted)
	noErr(t, err, "исполнение")
	resp.Body[0] ^= 0xff
	res.Response.Body[1] ^= 0xff

	replay, err := f.do(t, req, e.respond(created(2)), idem.OutcomeReplayed)
	noErr(t, err, "повтор")
	sameResponse(t, replay.Response, created(1), "запись после правки ответа op")
	replay.Response.Body[0] ^= 0xff
	f.replayed(t, req, created(1))
}

func suitePanic(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	req := f.request(t, "panic")
	recovered := func() (value any) {
		defer func() { value = recover() }()
		_, _ = f.sub.Do(t.Context(), req, func(context.Context) (idem.Response, error) {
			panic("idemtest suite: op panics")
		})
		return nil
	}()
	isTrue(t, recovered != nil, "паника op не дошла до вызывающего")

	f.executed(t, req, &e, created(1))
}

func suiteInvalidRequests(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	valid := f.request(t, "invalid")
	for _, tc := range []struct {
		what   string
		change func(r *idem.Request)
		want   error
	}{
		{"пустая область", func(r *idem.Request) { r.Scope = idem.Scope{} }, idem.ErrInvalidScope},
		{"реалм не по форме", func(r *idem.Request) { r.Scope.Realm = "Staff" }, idem.ErrInvalidScope},
		{"субъект с переводом строки", func(r *idem.Request) { r.Scope.Subject = "a\nb" }, idem.ErrInvalidScope},
		{"операция вне Config", func(r *idem.Request) { r.Operation = "orders.delete" }, idem.ErrInvalidRequest},
		{"ключ не разобран", func(r *idem.Request) { r.Key = idem.Key{} }, idem.ErrInvalidRequest},
		{"метод PUT", func(r *idem.Request) { r.Method = http.MethodPut }, idem.ErrInvalidRequest},
	} {
		req := valid
		tc.change(&req)
		_, err := f.sub.Do(t.Context(), req, e.respond(created(1)))
		errIs(t, err, tc.want, tc.what)
	}
	equal(t, e.calls.Load(), int64(0), "op на неверном запросе")
	equal(t, len(f.obs.Outcomes()), 0, "исходов у наблюдателя на неверных запросах")
	f.executed(t, valid, &e, created(1))
}

func suiteConfigCopied(t *testing.T, f *fixture) {
	t.Helper()
	f.cfg.Operations[0] = "orders.mutated"
	var e effect
	f.executed(t, f.request(t, "config"), &e, created(1))
}
