package inboxtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
)

// VerifierFixture — верификатор проекта и подписант его схемы (ADR-0012,
// решение 14). Подписант знает секреты отправителя; мир фикстуры — например,
// фейк API для перечитывания — знает только события, подписанные в сценарии.
type VerifierFixture struct {
	// Source — источник, который обслуживает верификатор.
	Source   inbox.SourceName
	Verifier inbox.Verifier
	// Sign — валидная доставка события n, подписанная в момент at действующим
	// секретом; разные n — разные события. RemoteIP кладёт только схема,
	// проверяющая адрес: набор тогда проверяет и его.
	Sign func(n int, at time.Time) inbox.Request
	// SignOther — та же доставка вторым действующим секретом (смена секрета);
	// nil — секрет у схемы один.
	SignOther func(n int, at time.Time) inbox.Request
	// Tolerance — допуск верификатора по времени; ноль — время схема не
	// подписывает, и подпись месячной давности годна.
	Tolerance time.Duration
	// Drift — вторая доставка события n с другими байтами перечитанного
	// объекта (решение 4); nil — у схемы подпись по байтам.
	Drift func(n int, at time.Time) inbox.Request
	// BodyUnsigned — тело подписью не защищено (постоянный секрет в заголовке,
	// как у Telegram): набор меняет только заголовки и адрес. Остальным схемам
	// — false: изменённый байт тела обязан дать отказ или то же событие.
	BodyUnsigned bool
}

// VerifierFactory — свежая фикстура на каждый сценарий; now — часы, которыми
// верификатор проверяет время подписи.
type VerifierFactory func(t *testing.T, now func() time.Time) VerifierFixture

// RunVerifierSuite — набор для верификатора, который пишет проект: порт
// проекта проверяется набором блока (ADR-0010, «Граница»).
func RunVerifierSuite(t *testing.T, newFixture VerifierFactory) {
	t.Helper()
	if newFixture == nil {
		panic("inboxtest.RunVerifierSuite: newFixture must not be nil")
	}
	for _, sc := range verifierScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			clock := NewClock(suiteNow)
			fx := newFixture(t, clock.Now)
			if fx.Verifier == nil || fx.Sign == nil || !fx.Source.Valid() {
				t.Fatalf("фикстура без верификатора, подписанта или с негодным источником %q", fx.Source)
			}
			sc.run(t, verifierCase{fx: fx, clock: clock})
		})
	}
}

type verifierScenario struct {
	name string
	run  func(t reporter, c verifierCase)
}

var verifierScenarios = []verifierScenario{
	{name: "валидная доставка — событие источника с ключом и типом", run: verifyValid},
	{name: "повтор доставки — тот же ключ и отпечаток; другое событие — другой ключ", run: verifyStableKey},
	{name: "сервис принимает событие, повтор — duplicate", run: verifyThroughService},
	{name: "изменён байт тела — отказ подлинности или то же событие", run: verifyBodyTamper},
	{name: "тело-мусор — отказ подлинности", run: verifyBodyGarbage},
	{name: "подписи нет или она мусор — отказ, а не паника и не ErrUnavailable", run: verifyHeaderGarbage},
	{name: "адрес — от периметра", run: verifyAddress},
	{name: "время вне допуска — отказ, внутри — событие", run: verifyTolerance},
	{name: "второй действующий секрет", run: verifyOtherSecret},
	{name: "дрейф перечитанного объекта — тот же ключ и отпечаток", run: verifyDrift},
	{name: "запрос не изменён и не удержан", run: verifyOwnership},
	{name: "параллельные вызовы", run: verifyParallel},
}

// verifierCase — фикстура сценария и часы её верификатора.
type verifierCase struct {
	fx    VerifierFixture
	clock *Clock
}

func verifyValid(t reporter, c verifierCase) {
	t.Helper()
	ev := c.mustVerify(t, c.fx.Sign(1, suiteNow), "валидная доставка")
	equal(t, ev.Source, c.fx.Source, "источник события")
	isTrue(t, inbox.ValidEventID(ev.ID), fmt.Sprintf("ключ %q не видимый ASCII 1..%d байт", clip(ev.ID), inbox.MaxEventIDLen))
	isTrue(t, ev.Type.Valid(), fmt.Sprintf("тип %q не [A-Za-z0-9_.:-]{1,%d}", clip(string(ev.Type)), inbox.MaxEventTypeLen))
	isTrue(t, ev.Digest == nil || len(ev.Digest) == inbox.DigestSize, "отпечаток не nil и не 32 байта")
	isTrue(t, len(ev.Payload) <= inbox.MaxPayloadBytes, "тело события больше 1 МиБ")
}

func verifyStableKey(t reporter, c verifierCase) {
	t.Helper()
	first := c.mustVerify(t, c.fx.Sign(1, suiteNow), "первая доставка")
	again := c.mustVerify(t, c.fx.Sign(1, suiteNow.Add(time.Second)), "повтор той же доставки секундой позже")
	sameDelivery(t, again, first, "повтор доставки")
	other := c.mustVerify(t, c.fx.Sign(2, suiteNow), "другое событие")
	isTrue(t, other.ID != first.ID, "у разных событий один ключ: второе молча сочтётся дублем")
}

func verifyThroughService(t reporter, c verifierCase) {
	t.Helper()
	req := c.fx.Sign(1, suiteNow)
	ev := c.mustVerify(t, req, "доставка для сервиса")
	if !ev.Type.Valid() {
		t.Fatalf("тип %q негоден: сервис его не объявит", clip(string(ev.Type)))
	}
	store := NewMemStore(map[inbox.SourceName]Handler{
		c.fx.Source: HandlerFunc(func(context.Context, inbox.Event) error { return nil }),
	})
	svc := inbox.NewService(store, NewObserver(), inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			c.fx.Source: {Verifier: c.fx.Verifier, Handle: []inbox.EventType{ev.Type}},
		},
		MaxBodyBytes: inbox.MaxPayloadBytes, Retention: 30 * 24 * time.Hour, PayloadRetention: 72 * time.Hour, PurgeBatch: 100,
	})
	svc.SetClock(c.clock.Now)

	receipt, err := svc.Receive(t.Context(), c.fx.Source, req)
	noErr(t, err, "сервис отверг валидную доставку")
	equal(t, receipt.Outcome, inbox.OutcomeAccepted, "исход валидной доставки")
	receipt, err = svc.Receive(t.Context(), c.fx.Source, c.fx.Sign(1, suiteNow))
	noErr(t, err, "сервис отверг повтор доставки")
	equal(t, receipt.Outcome, inbox.OutcomeDuplicate, "исход повтора: другой отпечаток дал бы conflict")
}

func verifyTolerance(t reporter, c verifierCase) {
	t.Helper()
	want := c.mustVerify(t, c.fx.Sign(1, suiteNow), "доставка в момент подписи")
	tolerance := c.fx.Tolerance
	if tolerance <= 0 {
		old := c.fx.Sign(1, suiteNow.Add(-30*24*time.Hour))
		sameDelivery(t, c.mustVerify(t, old, "подпись месячной давности у схемы без времени"), want,
			"схема без времени (Tolerance = 0)")
		return
	}
	for _, shift := range []time.Duration{-tolerance - time.Second, tolerance + time.Second} {
		c.mustRefuse(t, c.fx.Sign(1, suiteNow.Add(shift)), fmt.Sprintf("подпись со сдвигом %s при допуске %s", shift, tolerance))
	}
	for _, shift := range []time.Duration{-tolerance + time.Second, tolerance - time.Second} {
		got := c.mustVerify(t, c.fx.Sign(1, suiteNow.Add(shift)), fmt.Sprintf("подпись со сдвигом %s внутри допуска", shift))
		sameDelivery(t, got, want, "подпись внутри допуска")
	}
}

func verifyOtherSecret(t reporter, c verifierCase) {
	t.Helper()
	if c.fx.SignOther == nil {
		return
	}
	req, other := c.fx.Sign(1, suiteNow), c.fx.SignOther(1, suiteNow)
	isTrue(t, !sameRequest(req, other), "SignOther подписал так же, как Sign: второй секрет не проверяется")
	sameDelivery(t, c.mustVerify(t, other, "подпись вторым действующим секретом"), c.mustVerify(t, req, "подпись первым секретом"),
		"смена секрета")
}

func verifyDrift(t reporter, c verifierCase) {
	t.Helper()
	if c.fx.Drift == nil {
		return
	}
	first := c.mustVerify(t, c.fx.Sign(1, suiteNow), "доставка до дрейфа")
	drifted := c.mustVerify(t, c.fx.Drift(1, suiteNow), "доставка после дрейфа")
	isTrue(t, !bytes.Equal(first.Payload, drifted.Payload), "Drift не дрейфует: тело события то же, сценарий ничего не доказывает")
	equal(t, drifted.ID, first.ID, "ключ после дрейфа")
	equal(t, drifted.Type, first.Type, "тип после дрейфа")
	isTrue(t, bytes.Equal(digestOf(drifted), digestOf(first)),
		"дрейф поменял отпечаток: отпечаток по телу даёт ложный conflict на каждом повторе — считай Digest по типу, объекту, статусу и сумме")
}

func verifyOwnership(t reporter, c verifierCase) {
	t.Helper()
	req := c.fx.Sign(1, suiteNow)
	before := cloneRequest(req)
	ev := c.mustVerify(t, req, "доставка")
	kept := cloneEvent(ev)
	isTrue(t, sameRequest(req, before), "Verify изменил запрос")

	for i := range req.Raw {
		req.Raw[i] = '#'
	}
	for _, values := range req.Headers {
		for i := range values {
			values[i] = "scribbled"
		}
	}
	isTrue(t, sameEvent(ev, kept), "событие держит память запроса: правка запроса после Verify поменяла событие")
	sameDelivery(t, c.mustVerify(t, c.fx.Sign(1, suiteNow), "повторный вызов"), kept, "повторный вызов на той же доставке")
}

// mustVerify — событие без ошибки и без паники.
func (c verifierCase) mustVerify(t reporter, req inbox.Request, what string) inbox.Event {
	t.Helper()
	r := c.verify(t, req, what)
	if r.panicked || r.err != nil {
		t.Fatalf("%s: верификатор отказал валидной доставке: %v", what, r.err)
	}
	return r.ev
}

// mustRefuse — отказ подлинности, а не событие.
func (c verifierCase) mustRefuse(t reporter, req inbox.Request, what string) {
	t.Helper()
	r := c.verify(t, req, what)
	if !r.panicked && r.err == nil {
		t.Errorf("%s: ожидался отказ inbox.ErrNotAuthentic, верификатор принял событие", what)
		return
	}
	c.checkRefusal(t, r, req, what)
}

// refusedOrSame — изменённая доставка отказана подлинностью либо дала то же
// событие: другое принятое событие значит, что изменённое подписью не покрыто.
func (c verifierCase) refusedOrSame(t reporter, want inbox.Event, req inbox.Request, what string) {
	t.Helper()
	r := c.verify(t, req, what)
	if !r.panicked && r.err == nil {
		isTrue(t, sameEvent(r.ev, want), what+": событие принято другим — изменённое не покрыто подписью, а в событие попало")
		return
	}
	c.checkRefusal(t, r, req, what)
}

// checkRefusal — отказ ровно ErrNotAuthentic, а в тексте нет ни тела, ни
// длинных значений заголовков: текст ошибки уходит в лог.
func (c verifierCase) checkRefusal(t reporter, r verified, req inbox.Request, what string) {
	t.Helper()
	if r.panicked {
		return
	}
	notAuthentic := errors.Is(r.err, inbox.ErrNotAuthentic) &&
		!errors.Is(r.err, inbox.ErrUnavailable) && !errors.Is(r.err, inbox.ErrMalformed)
	isTrue(t, notAuthentic, fmt.Sprintf("%s: ожидался отказ только inbox.ErrNotAuthentic, получено %v", what, r.err))
	text := r.err.Error()
	if len(req.Raw) >= 16 && strings.Contains(text, string(req.Raw)) {
		t.Errorf("%s: текст отказа цитирует тело доставки", what)
	}
	for name, values := range req.Headers {
		for _, value := range values {
			if len(value) >= 32 && strings.Contains(text, value) {
				t.Errorf("%s: текст отказа цитирует заголовок %s", what, name)
			}
		}
	}
}

// verified — итог одного вызова Verify.
type verified struct {
	ev       inbox.Event
	err      error
	panicked bool
}

// verify — Verify с перехватом паники: паника верификатора — находка набора, а
// не падение всего прогона.
func (c verifierCase) verify(t reporter, req inbox.Request, what string) (r verified) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s: Verify паникует: %v", what, p)
			r = verified{panicked: true}
		}
	}()
	ev, err := c.fx.Verifier.Verify(t.Context(), req)
	return verified{ev: ev, err: err}
}

// sameEvent — события совпадают целиком, отпечаток — после разбора ядром.
func sameEvent(a, b inbox.Event) bool {
	return sameContent(a, b) && a.OccurredAt.Equal(b.OccurredAt)
}

// sameDelivery — та же доставка, подписанная в другой момент: момент подписи
// законно другой, остальное — нет.
func sameDelivery(t reporter, got, want inbox.Event, what string) {
	t.Helper()
	isTrue(t, sameContent(got, want),
		fmt.Sprintf("%s: другое событие — ключ %q против %q, тип %q против %q, тело или отпечаток разошлись",
			what, clip(got.ID), clip(want.ID), clip(string(got.Type)), clip(string(want.Type))))
}

func sameContent(a, b inbox.Event) bool {
	return a.Source == b.Source && a.ID == b.ID && a.Type == b.Type &&
		bytes.Equal(a.Payload, b.Payload) && bytes.Equal(digestOf(a), digestOf(b))
}

// digestOf — отпечаток так, как его посчитает ядро.
func digestOf(ev inbox.Event) []byte {
	if ev.Digest != nil {
		return ev.Digest
	}
	sum := sha256.Sum256(ev.Payload)
	return sum[:]
}

// clip — значение в сообщение набора без простыни.
func clip(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
