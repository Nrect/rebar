package inboxhttp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxhttp"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/kit/errs"
)

const (
	billing  inbox.SourceName = "billing"
	typePaid inbox.EventType  = "invoice.paid"
	maxBody                   = 256
)

var (
	start  = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	secret = []byte("inboxhttp-test-secret-0123456789")
)

// stand — ручка на двойниках; handle — решение обработчика, logs — лог ручки.
type stand struct {
	handler http.Handler
	store   *inboxtest.MemStore
	obs     *inboxtest.Observer
	remote  *remoteSeen
	logs    *syncBuffer
	handle  func(ctx context.Context, ev inbox.Event) error
	mu      sync.Mutex
}

func newStand(t *testing.T) *stand {
	t.Helper()
	s := &stand{obs: inboxtest.NewObserver(), remote: &remoteSeen{}, logs: &syncBuffer{}}
	s.store = inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		billing: inboxtest.HandlerFunc(func(ctx context.Context, ev inbox.Event) error {
			s.mu.Lock()
			handle := s.handle
			s.mu.Unlock()
			if handle == nil {
				return nil
			}
			return handle(ctx, ev)
		}),
	})
	clock := func() time.Time { return start }
	verifier := s.remote.wrap(inboxtest.NewHMACVerifier(billing, 5*time.Minute, clock, secret))
	svc := inbox.NewService(s.store, s.obs, inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			billing: {
				Verifier: verifier, Handle: []inbox.EventType{typePaid}, Ignore: []inbox.EventType{"invoice.viewed"},
				Ack: inbox.Ack{ContentType: "application/json", Body: []byte(`{"code":0}`)},
			},
		},
		MaxBodyBytes: maxBody, Retention: 30 * 24 * time.Hour, PayloadRetention: 72 * time.Hour, PurgeBatch: 100,
	})
	svc.SetClock(clock)
	s.handler = inboxhttp.New(svc, billing, inboxhttp.Config{
		RemoteIP:  func(r *http.Request) string { return r.Header.Get("X-Perimeter-Addr") },
		RequestID: func(context.Context) string { return "req-1" },
		Logger:    slog.New(slog.NewJSONHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return s
}

func (s *stand) setHandle(handle func(ctx context.Context, ev inbox.Event) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle = handle
}

func (s *stand) post(req inbox.Request) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/billing", bytes.NewReader(req.Raw))
	for name, values := range req.Headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	r.Header.Set("X-Perimeter-Addr", "185.71.76.1")
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	return w
}

func signed(id string, typ inbox.EventType) inbox.Request {
	return inboxtest.SignHMAC(secret, start, inboxtest.EventBody(id, typ, map[string]string{"phone": "+79990001122"}))
}

// Статус на каждый исход — таблица решения 7: 200 ровно учтённому и
// необрабатываемому, остальному — класс ошибки слагом; 410 не отдаётся никогда.
func TestHandler_StatusByOutcome(t *testing.T) {
	t.Parallel()

	noop := func() {}
	for _, tc := range []struct {
		outcome inbox.Outcome
		status  int
		slug    string
		prepare func(s *stand) (inbox.Request, func())
	}{
		{inbox.OutcomeAccepted, http.StatusOK, "", func(*stand) (inbox.Request, func()) { return signed("evt_1", typePaid), noop }},
		{inbox.OutcomeDuplicate, http.StatusOK, "", func(s *stand) (inbox.Request, func()) {
			s.post(signed("evt_1", typePaid))
			return signed("evt_1", typePaid), noop
		}},
		{inbox.OutcomeConflict, http.StatusOK, "", func(s *stand) (inbox.Request, func()) {
			s.post(signed("evt_1", typePaid))
			return inboxtest.SignHMAC(secret, start, inboxtest.EventBody("evt_1", typePaid, "другое")), noop
		}},
		{inbox.OutcomeIgnored, http.StatusOK, "", func(*stand) (inbox.Request, func()) {
			return signed("evt_2", "invoice.viewed"), noop
		}},
		{inbox.OutcomeUnknownType, http.StatusServiceUnavailable, "unavailable", func(*stand) (inbox.Request, func()) {
			return signed("evt_3", "invoice.voided"), noop
		}},
		{inbox.OutcomeInFlight, http.StatusConflict, "conflict", inFlight},
		{inbox.OutcomeNotAuthentic, http.StatusBadRequest, "incorrect-input", func(*stand) (inbox.Request, func()) {
			return inboxtest.SignHMAC([]byte("forged"), start, inboxtest.EventBody("evt_4", typePaid, nil)), noop
		}},
		{inbox.OutcomeMalformed, http.StatusServiceUnavailable, "unavailable", func(*stand) (inbox.Request, func()) {
			return inboxtest.SignHMAC(secret, start, []byte(`{"type":"invoice.paid"}`)), noop
		}},
		{inbox.OutcomeTooLarge, http.StatusRequestEntityTooLarge, "payload-too-large", func(*stand) (inbox.Request, func()) {
			return inboxtest.SignHMAC(secret, start, bytes.Repeat([]byte("x"), maxBody+1)), noop
		}},
		{inbox.OutcomeError, http.StatusServiceUnavailable, "unavailable", func(s *stand) (inbox.Request, func()) {
			s.store.SetErr(errors.New("connection refused"))
			return signed("evt_5", typePaid), noop
		}},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			t.Parallel()
			s := newStand(t)
			req, done := tc.prepare(s)
			seen := len(s.obs.Deliveries())
			w := s.post(req)
			done()

			deliveries := s.obs.Deliveries()
			require.Greater(t, len(deliveries), seen, "доставка не дошла до сервиса")
			assert.Equal(t, tc.outcome, deliveries[seen].Outcome, "исход сервиса")
			assert.Equal(t, tc.status, w.Code, "статус исхода %s", tc.outcome)
			assert.NotEqual(t, http.StatusGone, w.Code, "410 — просьба отключить приёмник")
			if tc.status == http.StatusOK {
				assert.Equal(t, `{"code":0}`, w.Body.String(), "подтверждение источника")
				assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
				return
			}
			assert.JSONEq(t, `{"slug":"`+tc.slug+`","request_id":"req-1"}`, w.Body.String(), "отказ классом, без словаря продукта")
		})
	}
}

// inFlight — первая доставка держит ключ в обработчике, пока проверяемая не
// ответит; done отпускает её и ждёт конца.
func inFlight(s *stand) (req inbox.Request, done func()) {
	req = signed("evt_busy", typePaid)
	release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.setHandle(func(context.Context, inbox.Event) error {
		close(entered)
		<-release
		return nil
	})
	go func() {
		defer close(finished)
		s.post(req)
	}()
	<-entered
	return req, func() {
		close(release)
		<-finished
	}
}

// Ошибка обработчика со слагом продукта остаётся 503: ответчик ручки без
// Translate, и класс обработчика не проступает сквозь ErrUnavailable.
func TestHandler_HandlerSlugErrorStays503(t *testing.T) {
	t.Parallel()

	s := newStand(t)
	s.setHandle(func(context.Context, inbox.Event) error { return errs.Conflict("seat-taken") })
	w := s.post(signed("evt_slug", typePaid))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"slug":"unavailable","request_id":"req-1"}`, w.Body.String())
}

// Потолок: ровно MaxBodyBytes проверяется подписью, байт сверх — 413 без
// проверки; ручка не читает дальше потолка плюс байт.
func TestHandler_BodyLimit(t *testing.T) {
	t.Parallel()

	s := newStand(t)
	exact := s.post(inboxtest.SignHMAC([]byte("forged"), start, bytes.Repeat([]byte("x"), maxBody)))
	assert.Equal(t, http.StatusBadRequest, exact.Code, "тело ровно в потолок дошло до подписи")
	assert.Equal(t, 1, s.remote.calls(), "верификатор звали")

	huge := &countingReader{remaining: 10 * maxBody}
	r := httptest.NewRequest(http.MethodPost, "/webhooks/billing", huge)
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, 1, s.remote.calls(), "тело сверх потолка не дошло до верификатора")
	assert.Equal(t, maxBody+1, huge.read, "прочитано больше потолка плюс байт")
}

// Адрес — из функции периметра, а не r.RemoteAddr; заголовки доходят до
// верификатора в канонической форме.
func TestHandler_RemoteIPAndHeaders(t *testing.T) {
	t.Parallel()

	s := newStand(t)
	w := s.post(signed("evt_addr", typePaid))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "185.71.76.1", s.remote.last())
	assert.NotEmpty(t, s.remote.header(inboxtest.SignatureHeader))
}

// В лог ручки не уходит ни тело, ни подпись: ошибка пишется текстом ядра.
func TestHandler_LogsNoBodyOrSignature(t *testing.T) {
	t.Parallel()

	s := newStand(t)
	s.store.SetErr(errors.New("connection refused"))
	req := signed("evt_logged", typePaid)
	w := s.post(req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	logged := s.logs.String()
	require.Contains(t, logged, `"status":503`, "запись об ошибке есть")
	for _, part := range []string{"+79990001122", "evt_logged", req.Headers[inboxtest.SignatureHeader][0]} {
		assert.NotContains(t, logged, part)
	}
}

// Недочитанное тело — 400 классом, до сервиса не доходит.
func TestHandler_UnreadableBody(t *testing.T) {
	t.Parallel()

	s := newStand(t)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/billing", failingReader{})
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, `{"slug":"incorrect-input","request_id":"req-1"}`, w.Body.String())
	assert.Empty(t, s.obs.Deliveries())
	assert.Zero(t, s.remote.calls())
}

// Пустое подтверждение — 200 без тела и без Content-Type.
func TestHandler_EmptyAck(t *testing.T) {
	t.Parallel()

	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		billing: inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return nil }),
	})
	clock := func() time.Time { return start }
	svc := inbox.NewService(store, inboxtest.NewObserver(), inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			billing: {Verifier: inboxtest.NewHMACVerifier(billing, time.Minute, clock, secret), Handle: []inbox.EventType{typePaid}},
		},
		MaxBodyBytes: maxBody, Retention: time.Hour, PayloadRetention: time.Hour, PurgeBatch: 1,
	})
	svc.SetClock(clock)
	handler := inboxhttp.New(svc, billing, inboxhttp.Config{RemoteIP: func(*http.Request) string { return "" }})

	req := signed("evt_empty", typePaid)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/billing", bytes.NewReader(req.Raw))
	r.Header.Set(inboxtest.SignatureHeader, req.Headers[inboxtest.SignatureHeader][0])
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Body.String())
	assert.Empty(t, w.Header().Get("Content-Type"))
}

func TestNew_Panics(t *testing.T) {
	t.Parallel()

	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		billing: inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return nil }),
	})
	clock := func() time.Time { return start }
	svc := inbox.NewService(store, inboxtest.NewObserver(), inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			billing: {Verifier: inboxtest.NewHMACVerifier(billing, time.Minute, clock, secret), Handle: []inbox.EventType{typePaid}},
		},
		MaxBodyBytes: maxBody, Retention: time.Hour, PayloadRetention: time.Hour, PurgeBatch: 1,
	})
	remote := func(*http.Request) string { return "" }
	assert.PanicsWithValue(t, "inboxhttp.New: service must not be nil",
		func() { inboxhttp.New(nil, billing, inboxhttp.Config{RemoteIP: remote}) })
	assert.PanicsWithValue(t, `inboxhttp.New: source "delivery" must be declared in the service config`,
		func() { inboxhttp.New(svc, "delivery", inboxhttp.Config{RemoteIP: remote}) })
	assert.PanicsWithValue(t, "inboxhttp.New: Config.RemoteIP must not be nil: the sender address comes from the trusted perimeter",
		func() { inboxhttp.New(svc, billing, inboxhttp.Config{}) })
}

// remoteSeen — обёртка верификатора: что пришло в порт.
type remoteSeen struct {
	mu      sync.Mutex
	n       int
	remote  string
	headers map[string][]string
}

func (r *remoteSeen) wrap(next inbox.Verifier) inbox.Verifier {
	return verifierFunc(func(ctx context.Context, req inbox.Request) (inbox.Event, error) {
		r.mu.Lock()
		r.n++
		r.remote, r.headers = req.RemoteIP, req.Headers
		r.mu.Unlock()
		return next.Verify(ctx, req)
	})
}

func (r *remoteSeen) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func (r *remoteSeen) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remote
}

func (r *remoteSeen) header(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.headers[name]
}

type verifierFunc func(ctx context.Context, req inbox.Request) (inbox.Event, error)

func (f verifierFunc) Verify(ctx context.Context, req inbox.Request) (inbox.Event, error) {
	return f(ctx, req)
}

// countingReader — бесконечное по меркам ручки тело, которое считает прочитанное.
type countingReader struct {
	remaining, read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.remaining)
	for i := range n {
		p[i] = 'x'
	}
	c.remaining -= n
	c.read += n
	return n, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// syncBuffer — буфер лога под замком: ручка пишет из горутины запроса.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Clone(b.buf.String())
}
