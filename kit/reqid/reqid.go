package reqid

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

const (
	// Header — заголовок идентификатора запроса, он же имя эха в ответе.
	Header = "X-Request-Id"
	// MaxLen — потолок длины принимаемого извне значения.
	MaxLen = 128
)

// ctxKey — собственный тип ключа: чужой пакет не перезапишет значение.
type ctxKey struct{}

// Middleware — принять X-Request-Id клиента, если он в безопасной форме,
// иначе выдать свой; положить в контекст и вернуть эхом в ответе.
func Middleware(next http.Handler) http.Handler {
	if next == nil {
		panic("reqid.Middleware: next must not be nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(Header)
		if !valid(id) {
			id = newID()
		}
		w.Header().Set(Header, id)
		next.ServeHTTP(w, r.WithContext(With(r.Context(), id)))
	})
}

// From — идентификатор запроса; "" вне запроса.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// With — контекст с идентификатором: для фоновых задач и тестов, где
// Middleware не работал.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// valid — [A-Za-z0-9._-], от одного символа до MaxLen. Набор узкий нарочно:
// значение уходит в лог, в заголовок ответа и в тело ошибки, а CR, LF и
// кавычка в любом из этих мест — инъекция.
func valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}
	for i := range len(id) {
		if !safeByte(id[i]) {
			return false
		}
	}
	return true
}

// safeByte — один байт разрешённого алфавита.
func safeByte(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return c == '.' || c == '_' || c == '-'
}

// newID — 16 случайных байт в base64url без выравнивания (22 символа из
// того же алфавита, что принимает valid).
func newID() string {
	var buf [16]byte
	// crypto/rand.Read ошибку не возвращает: при отказе источника энтропии
	// процесс падает сам.
	_, _ = rand.Read(buf[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}
