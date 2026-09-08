package reqid_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/reqid"
)

// safeForm — то, что разрешено видеть в логе и в заголовке ответа.
var safeForm = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// serve — прогон одного запроса через middleware; возвращает id, увиденный
// хендлером, и ответ.
func serve(t *testing.T, incoming string) (string, *httptest.ResponseRecorder) {
	t.Helper()

	seen := ""
	handler := reqid.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = reqid.From(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	if incoming != "" {
		req.Header[reqid.Header] = []string{incoming}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return seen, rec
}

// Годный чужой идентификатор принимается как есть: по нему клиент склеивает
// свои логи с нашими.
func TestMiddleware_AcceptsSafeIncoming(t *testing.T) {
	t.Parallel()

	for _, incoming := range []string{
		"abc123",
		"01H8XG7-9K.QW_ER",
		"a",
		strings.Repeat("x", reqid.MaxLen),
	} {
		seen, rec := serve(t, incoming)

		assert.Equal(t, incoming, seen)
		assert.Equal(t, incoming, rec.Header().Get(reqid.Header), "эхо в ответе")
	}
}

// Мусорный заголовок заменяется своим, а не роняет запрос: CR и LF разрезали
// бы строку лога и заголовки ответа.
func TestMiddleware_ReplacesUnsafeIncoming(t *testing.T) {
	t.Parallel()

	for name, incoming := range map[string]string{
		"перевод строки":  "abc\r\nX-Admin: 1",
		"пробел":          "abc def",
		"кавычка":         `abc"def`,
		"юникод":          "запрос-1",
		"слишком длинный": strings.Repeat("x", reqid.MaxLen+1),
		"отсутствует":     "",
		"один пробел":     " ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			seen, rec := serve(t, incoming)

			assert.NotEqual(t, incoming, seen)
			assert.Regexp(t, safeForm, seen, "выданный id безопасен по форме")
			assert.Equal(t, seen, rec.Header().Get(reqid.Header))
		})
	}
}

// 16 байт в base64url — 22 символа; два запроса не получают один id.
func TestMiddleware_GeneratedIDsAreRandom(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 64)
	for range 64 {
		id, _ := serve(t, "")
		require.Len(t, id, 22)
		require.False(t, seen[id], "повтор идентификатора %q", id)
		seen[id] = true
	}
}

// Границы алфавита поимённо: сдвиг любой из них проходит мимо тестов на
// «abc123» и на CRLF, а алфавит — это и есть защита от инъекции.
func TestMiddleware_AlphabetEdges(t *testing.T) {
	t.Parallel()

	for _, incoming := range []string{"a", "z", "A", "Z", "0", "9", ".", "_", "-", "aZ0._-"} {
		seen, _ := serve(t, incoming)
		assert.Equalf(t, incoming, seen, "%q законен", incoming)
	}
	// Соседи границ по коду: ` {  @ [  / :  , ^
	for _, incoming := range []string{"`", "{", "@", "[", "/", ":", ",", "^", "+", "%"} {
		seen, _ := serve(t, incoming)
		assert.NotEqualf(t, incoming, seen, "%q не должен проходить", incoming)
	}
}

// Свойство целиком: что бы ни прислал клиент, наружу и в лог уходит только
// безопасная форма, а подмена означает выданный нами идентификатор.
func FuzzMiddleware_IDIsAlwaysSafe(f *testing.F) {
	for _, seed := range []string{
		"", "abc123", "a b", "a\r\nX-Admin: 1", "юникод", ".", "-", strings.Repeat("x", 129),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, incoming string) {
		seen, rec := serve(t, incoming)

		require.Regexp(t, safeForm, seen)
		require.Equal(t, seen, rec.Header().Get(reqid.Header))
		if seen != incoming {
			require.Len(t, seen, 22, "чужое значение отвергнуто — значит выдан свой id")
		}
	})
}

func TestMiddleware_PanicsOnNilNext(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "reqid.Middleware: next must not be nil", func() {
		reqid.Middleware(nil)
	})
}

func TestFromAndWith(t *testing.T) {
	t.Parallel()

	assert.Empty(t, reqid.From(context.Background()), "вне запроса идентификатора нет")
	assert.Equal(t, "job-42", reqid.From(reqid.With(t.Context(), "job-42")))
}
