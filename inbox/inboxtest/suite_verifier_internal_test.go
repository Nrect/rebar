package inboxtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/inbox"
)

// Набор, который не видит сломанный верификатор, хуже отсутствующего: каждая
// поломка ниже — ловушка из решений 3 и 4 ADR-0012, и набор обязан назвать её.
// Контроль — честный верификатор тех же фикстур без находок.
func TestRunVerifierSuite_CatchesBrokenVerifiers(t *testing.T) {
	t.Parallel()

	for _, tc := range brokenVerifiers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := NewClock(suiteNow)
			honest := tc.fixture(clock.Now)
			assert.Empty(t, scenarioFindings(tc.run, honest, clock), "контроль: честный верификатор фикстуры без находок")

			broken := honest
			broken.Verifier = tc.breaks(honest.Verifier, clock.Now)
			if tc.fixtureBreaks != nil {
				broken = tc.fixtureBreaks(honest)
			}
			found := scenarioFindings(tc.run, broken, clock)
			assert.True(t, containsFinding(found, tc.want), "набор не назвал поломку %q; находки: %v", tc.want, found)
		})
	}
}

type brokenVerifier struct {
	name          string
	run           func(reporter, verifierCase)
	fixture       func(now func() time.Time) VerifierFixture
	breaks        func(honest inbox.Verifier, now func() time.Time) inbox.Verifier
	fixtureBreaks func(honest VerifierFixture) VerifierFixture
	want          string
}

func brokenVerifiers() []brokenVerifier {
	keep := func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier { return honest }
	return []brokenVerifier{
		{
			name: "подпись не проверяется", run: verifyBodyTamper, fixture: hmacFixture,
			breaks: func(inbox.Verifier, func() time.Time) inbox.Verifier { return verifierFunc(unsignedParse) },
			want:   "не покрыто подписью",
		},
		{
			name: "текст отказа цитирует тело", run: verifyBodyTamper, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					if err != nil {
						return ev, fmt.Errorf("%w: body %s", err, req.Raw)
					}
					return ev, nil
				})
			},
			want: "цитирует тело",
		},
		{
			name: "паника без подписи", run: verifyHeaderGarbage, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					_ = req.Headers[SignatureHeader][0]
					return honest.Verify(ctx, req)
				})
			},
			want: "паникует",
		},
		{
			name: "мусор подписи — ErrUnavailable", run: verifyHeaderGarbage, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					if err != nil {
						return ev, fmt.Errorf("%w: %w", inbox.ErrUnavailable, err)
					}
					return ev, nil
				})
			},
			want: "ожидался отказ только inbox.ErrNotAuthentic",
		},
		{
			name: "тело события — память запроса", run: verifyOwnership, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					ev.Payload = req.Raw
					return ev, err
				})
			},
			want: "держит память запроса",
		},
		{
			name: "Verify меняет запрос", run: verifyOwnership, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					delete(req.Headers, SignatureHeader)
					return ev, err
				})
			},
			want: "изменил запрос",
		},
		{
			name: "ключ зависит от момента подписи", run: verifyStableKey, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					ev.ID += fmt.Sprintf(":%d", ev.OccurredAt.Unix())
					return ev, err
				})
			},
			want: "повтор доставки",
		},
		{
			name: "у разных событий один ключ", run: verifyStableKey, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					ev.ID = "evt_same"
					return ev, err
				})
			},
			want: "один ключ",
		},
		{
			name: "ключ негоден для схемы", run: verifyThroughService, fixture: hmacFixture,
			breaks: func(honest inbox.Verifier, _ func() time.Time) inbox.Verifier {
				return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
					ev, err := honest.Verify(ctx, req)
					ev.ID = "evt with space"
					return ev, err
				})
			},
			want: "сервис отверг валидную доставку",
		},
		{
			name: "допуск шире объявленного", run: verifyTolerance, fixture: hmacFixture,
			breaks: func(_ inbox.Verifier, now func() time.Time) inbox.Verifier {
				return NewHMACVerifier("billing", time.Hour, now, testSecret, testOtherSecret)
			},
			want: "верификатор принял событие",
		},
		{
			name: "время проверяется, а допуск не объявлен", run: verifyTolerance, fixture: hmacFixture, breaks: keep,
			fixtureBreaks: func(honest VerifierFixture) VerifierFixture {
				honest.Tolerance = 0
				return honest
			},
			want: "отказал валидной доставке",
		},
		{
			name: "второй секрет не действует", run: verifyOtherSecret, fixture: hmacFixture,
			breaks: func(_ inbox.Verifier, now func() time.Time) inbox.Verifier {
				return NewHMACVerifier("billing", 5*time.Minute, now, testSecret)
			},
			want: "отказал валидной доставке",
		},
		{
			name: "отпечаток по дрейфующему телу", run: verifyDrift, fixture: driftFixture,
			breaks: func(_ inbox.Verifier, now func() time.Time) inbox.Verifier {
				return NewHMACVerifier("billing", 5*time.Minute, now, testSecret)
			},
			want: "дрейф поменял отпечаток",
		},
		{
			name: "Drift не дрейфует", run: verifyDrift, fixture: driftFixture, breaks: keep,
			fixtureBreaks: func(honest VerifierFixture) VerifierFixture {
				honest.Drift = honest.Sign
				return honest
			},
			want: "Drift не дрейфует",
		},
		{
			name: "адрес не проверяется", run: verifyAddress, fixture: addressFixture,
			breaks: func(_ inbox.Verifier, now func() time.Time) inbox.Verifier {
				return NewHMACVerifier("billing", 5*time.Minute, now, testSecret)
			},
			want: "ожидался отказ inbox.ErrNotAuthentic",
		},
		{
			name: "адрес без приведения IPv4 внутри IPv6", run: verifyAddress, fixture: addressFixture,
			breaks: func(_ inbox.Verifier, now func() time.Time) inbox.Verifier {
				return byAddress(NewHMACVerifier("billing", 5*time.Minute, now, testSecret), func(remote string) bool {
					addr, err := netip.ParseAddr(remote)
					return err == nil && senderNet.Contains(addr)
				})
			},
			want: "отказал валидной доставке",
		},
	}
}

var (
	testSecret      = []byte("inboxtest-internal-secret-0123456789")
	testOtherSecret = []byte("inboxtest-internal-other-secret-9876")
	senderNet       = netip.MustParsePrefix("185.71.76.0/27")
)

func suiteBody(n int, extra string) []byte {
	return EventBody(fmt.Sprintf("evt_%d", n), "invoice.paid", map[string]any{"invoice": n, "meta": extra})
}

func hmacFixture(now func() time.Time) VerifierFixture {
	return VerifierFixture{
		Source:    "billing",
		Verifier:  NewHMACVerifier("billing", 5*time.Minute, now, testSecret, testOtherSecret),
		Sign:      func(n int, at time.Time) inbox.Request { return SignHMAC(testSecret, at, suiteBody(n, "")) },
		SignOther: func(n int, at time.Time) inbox.Request { return SignHMAC(testOtherSecret, at, suiteBody(n, "")) },
		Tolerance: 5 * time.Minute,
	}
}

// driftFixture — «перечитанный» объект дрейфует полем meta; честный
// верификатор считает отпечаток по ключу и типу.
func driftFixture(now func() time.Time) VerifierFixture {
	honest := NewHMACVerifier("billing", 5*time.Minute, now, testSecret)
	return VerifierFixture{
		Source: "billing",
		Verifier: verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
			ev, err := honest.Verify(ctx, req)
			if err == nil {
				sum := sha256.Sum256([]byte(string(ev.Type) + "\x00" + ev.ID))
				ev.Digest = sum[:]
			}
			return ev, err
		}),
		Sign:      func(n int, at time.Time) inbox.Request { return SignHMAC(testSecret, at, suiteBody(n, "")) },
		Drift:     func(n int, at time.Time) inbox.Request { return SignHMAC(testSecret, at, suiteBody(n, "flag set")) },
		Tolerance: 5 * time.Minute,
	}
}

// addressFixture — схема с проверкой сети отправителя.
func addressFixture(now func() time.Time) VerifierFixture {
	return VerifierFixture{
		Source:   "billing",
		Verifier: byAddress(NewHMACVerifier("billing", 5*time.Minute, now, testSecret), func(remote string) bool { return inbox.AddrIn(remote, []netip.Prefix{senderNet}) }),
		Sign: func(n int, at time.Time) inbox.Request {
			req := SignHMAC(testSecret, at, suiteBody(n, ""))
			req.RemoteIP = "185.71.76.1"
			return req
		},
		Tolerance: 5 * time.Minute,
	}
}

func byAddress(next inbox.Verifier, allowed func(remote string) bool) inbox.Verifier {
	return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
		if !allowed(req.RemoteIP) {
			return inbox.Event{}, fmt.Errorf("%w: sender address is outside the sender networks", inbox.ErrNotAuthentic)
		}
		return next.Verify(ctx, req)
	})
}

// unsignedParse — верификатор, который забыл про подпись и разбирает тело.
func unsignedParse(_ context.Context, req inbox.Request) (inbox.Event, error) {
	var body struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Raw, &body) != nil {
		return inbox.Event{}, fmt.Errorf("%w: unreadable", inbox.ErrNotAuthentic)
	}
	return inbox.Event{Source: "billing", ID: body.ID, Type: inbox.EventType(body.Type), Payload: append([]byte(nil), req.Raw...)}, nil
}

type verifierFunc func(ctx context.Context, req inbox.Request) (inbox.Event, error)

func (f verifierFunc) Verify(ctx context.Context, req inbox.Request) (inbox.Event, error) {
	return f(ctx, req)
}

// recordingT — reporter, который запоминает находки набора вместо падения.
type recordingT struct {
	mu       sync.Mutex
	findings []string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Context() context.Context { return context.Background() }

func (r *recordingT) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.findings = append(r.findings, fmt.Sprintf(format, args...))
}

func (r *recordingT) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// scenarioFindings — находки одного сценария; Fatalf обрывает только его горутину.
func scenarioFindings(run func(reporter, verifierCase), fx VerifierFixture, clock *Clock) []string {
	rec := &recordingT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(rec, verifierCase{fx: fx, clock: clock})
	}()
	<-done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.findings
}

func containsFinding(findings []string, want string) bool {
	for _, finding := range findings {
		if strings.Contains(finding, want) {
			return true
		}
	}
	return false
}

// Паника фикстуры без верификатора — не находка, а отказ набора на входе.
func TestRunVerifierSuite_NilFactoryPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "inboxtest.RunVerifierSuite: newFixture must not be nil", func() { RunVerifierSuite(t, nil) })
	assert.PanicsWithValue(t, "inboxtest.RunStoreSuite: newStore must not be nil", func() { RunStoreSuite(t, nil) })
	assert.ErrorIs(t, ErrSchemaCheck, inbox.ErrUnavailable, "сбой схемы у двойника — как у адаптера")
}
