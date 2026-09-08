package ratelimithttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/ratelimit"
	"github.com/nrect/rebar/kit/ratelimit/ratelimithttp"
)

// gateFunc — двойник порта Gate: своя реализация в тесте вместо подпакета с
// двойниками, потому что настоящий Limiter уже in-memory (см. doc.go).
type gateFunc func(ctx context.Context, key string) (ratelimit.Decision, error)

func (f gateFunc) Allow(ctx context.Context, key string) (ratelimit.Decision, error) {
	return f(ctx, key)
}

func allowAll() gateFunc {
	return func(context.Context, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: true, Remaining: 7}, nil
	}
}

// serve — прогон одного запроса через middleware; возвращает ответ, факт
// вызова обработчика и решение, дошедшее до Deny.
func serve(t *testing.T, g ratelimit.Gate, key ratelimithttp.KeyFunc) (*httptest.ResponseRecorder, bool, ratelimit.Decision) {
	t.Helper()

	var denied ratelimit.Decision
	passed := false
	handler := ratelimithttp.Middleware(g, key, func(w http.ResponseWriter, _ *http.Request, d ratelimit.Decision) {
		denied = d
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"slug":"rate-limited"}`))
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		passed = true
		w.WriteHeader(http.StatusNoContent)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.RemoteAddr = "203.0.113.7:33333"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w, passed, denied
}

func staticKey(key string) ratelimithttp.KeyFunc {
	return func(*http.Request) string { return key }
}

func TestMiddleware_PassesAllowed(t *testing.T) {
	t.Parallel()

	w, passed, _ := serve(t, allowAll(), staticKey("ip"))

	assert.True(t, passed, "разрешённый запрос обязан дойти до обработчика")
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Header().Get("Retry-After"), "разрешённому запросу ждать нечего")
}

func TestMiddleware_DeniesAndSetsRetryAfter(t *testing.T) {
	t.Parallel()

	gate := gateFunc(func(context.Context, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{RetryAfter: 1200 * time.Millisecond}, nil
	})
	w, passed, denied := serve(t, gate, staticKey("ip"))

	assert.False(t, passed, "отказ не должен доходить до обработчика")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "2", w.Header().Get("Retry-After"), "секунды округляются вверх")
	assert.Equal(t, 1200*time.Millisecond, denied.RetryAfter, "решение доходит до Deny целиком")
}

// Ошибка гейта — отказ, даже если рядом с ней пришло разрешение: адаптер,
// упавший на середине, не имеет права пропускать трафик.
func TestMiddleware_GateErrorIsDenied(t *testing.T) {
	t.Parallel()

	gate := gateFunc(func(context.Context, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: true}, errors.New("redis недоступен")
	})
	w, passed, denied := serve(t, gate, staticKey("ip"))

	assert.False(t, passed, "fail-closed: ошибка гейта не пропускает запрос")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.False(t, denied.Allowed, "Deny обязан увидеть отказ, а не разрешение")
}

// Пустой ключ доходит до настоящего лимитера и превращается в отказ:
// проверяется вся связка, а не только middleware.
func TestMiddleware_EmptyKeyIsDenied(t *testing.T) {
	t.Parallel()

	limiter := ratelimit.New(ratelimit.Config{
		Limit: 10, Window: time.Minute, IdleTTL: time.Hour, MaxKeys: 16,
	})
	w, passed, denied := serve(t, limiter, staticKey(""))

	assert.False(t, passed)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.False(t, denied.Allowed)
	assert.Zero(t, limiter.Stats().Keys, "пустой ключ не заводит корзину")
}

// Кроме Retry-After middleware в ответ ничего не пишет: тело и статус —
// дело потребителя, а ключ (IP) наружу не уходит.
func TestMiddleware_WritesOnlyRetryAfter(t *testing.T) {
	t.Parallel()

	gate := gateFunc(func(context.Context, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{RetryAfter: time.Second}, nil
	})
	handler := ratelimithttp.Middleware(gate, staticKey("203.0.113.7"),
		func(http.ResponseWriter, *http.Request, ratelimit.Decision) {},
	)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.RemoteAddr = "203.0.113.7:33333"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, []string{"Retry-After"}, headerNames(w), "middleware ставит только Retry-After")
	assert.Empty(t, w.Body.String(), "тело пишет обработчик отказа")
	assert.NotContains(t, w.Header().Get("Retry-After"), "203.0.113.7", "ключ в ответ не попадает")
}

func headerNames(w *httptest.ResponseRecorder) []string {
	names := make([]string, 0, len(w.Header()))
	for name := range w.Header() {
		names = append(names, name)
	}
	return names
}

func TestMiddleware_PanicsOnNilParts(t *testing.T) {
	t.Parallel()

	deny := func(http.ResponseWriter, *http.Request, ratelimit.Decision) {}

	assert.PanicsWithValue(t, "ratelimithttp.Middleware: gate must not be nil",
		func() { ratelimithttp.Middleware(nil, staticKey("ip"), deny) })
	assert.PanicsWithValue(t, "ratelimithttp.Middleware: key must not be nil",
		func() { ratelimithttp.Middleware(allowAll(), nil, deny) })
	assert.PanicsWithValue(t, "ratelimithttp.Middleware: deny must not be nil",
		func() { ratelimithttp.Middleware(allowAll(), staticKey("ip"), nil) })
	assert.PanicsWithValue(t, "ratelimithttp.Middleware: next must not be nil",
		func() { ratelimithttp.Middleware(allowAll(), staticKey("ip"), deny)(nil) })
}

// Retry-After меньше секунды всё равно секунда: ноль в заголовке означал бы
// «прямо сейчас» и вернул бы клиента ровно в отказ.
func TestMiddleware_RoundsRetryAfterUp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		after time.Duration
		want  string
	}{
		{after: time.Nanosecond, want: "1"},
		{after: time.Second, want: "1"},
		{after: time.Second + time.Nanosecond, want: "2"},
		{after: 59 * time.Second, want: "59"},
		{after: 0, want: ""},
	} {
		gate := gateFunc(func(context.Context, string) (ratelimit.Decision, error) {
			return ratelimit.Decision{RetryAfter: tc.after}, nil
		})
		w, _, _ := serve(t, gate, staticKey("ip"))
		assert.Equalf(t, tc.want, w.Header().Get("Retry-After"), "срок %s", tc.after)
	}
}
