package ratelimithttp

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nrect/rebar/kit/ratelimit"
)

// retryAfterHeader — заголовок с честным сроком возврата.
const retryAfterHeader = "Retry-After"

// KeyFunc — ключ лимита из запроса. Пустой ключ означает «вычислить не
// удалось» и приводит к отказу, а не к пропуску.
type KeyFunc func(r *http.Request) string

// Deny — ответ на отказ: статус, тело и слаг пишет потребитель своим
// httperr. Retry-After к этому моменту уже выставлен.
type Deny func(w http.ResponseWriter, r *http.Request, d ratelimit.Decision)

// Middleware — проверка лимита перед обработчиком. Паникует на nil-аргументе:
// «лимитер не подключён» обязано падать на старте.
func Middleware(g ratelimit.Gate, key KeyFunc, deny Deny) func(http.Handler) http.Handler {
	if g == nil {
		panic("ratelimithttp.Middleware: gate must not be nil")
	}
	if key == nil {
		panic("ratelimithttp.Middleware: key must not be nil")
	}
	if deny == nil {
		panic("ratelimithttp.Middleware: deny must not be nil")
	}

	return func(next http.Handler) http.Handler {
		if next == nil {
			panic("ratelimithttp.Middleware: next must not be nil")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := g.Allow(r.Context(), key(r))
			if err == nil && d.Allowed {
				next.ServeHTTP(w, r)
				return
			}
			// Ошибка гейта — отказ, каким бы ни было решение рядом с ней.
			d.Allowed = false
			if d.RetryAfter > 0 {
				w.Header().Set(retryAfterHeader, strconv.Itoa(secondsUp(d.RetryAfter)))
			}
			deny(w, r, d)
		})
	}
}

// secondsUp — секунды с округлением вверх: 1.2s → 2, 1ns → 1.
func secondsUp(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}
