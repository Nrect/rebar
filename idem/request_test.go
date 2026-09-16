package idem_test

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

// ФОРМАТ ОТПЕЧАТКА — КОНТРАКТ СОВМЕСТИМОСТИ: отпечатки лежат в записях, и
// новый формат на выкате ответил бы 409 на законный повтор в пределах срока.
// Эталоны посчитаны вне Go (Python hashlib и struct.pack(">q")). Красный тест
// значит «останови выкат», а не «поправь эталон».
func TestFingerprint_GoldenVector(t *testing.T) {
	t.Parallel()

	req := idem.Request{
		Operation: "orders.create", Method: http.MethodPost, Path: "/orders/%41b",
		RawQuery: "source=web&x=1", Body: []byte(`{"product":"a"}`),
	}
	assert.Equal(t, "200832e71247fd3a0d511264045d4a71e7d047772927f2e99de4827b6d0151fc", hex.EncodeToString(req.Fingerprint()))
	assert.Equal(t, "6deaff92c5739dcdaa14ec0fe2c55f1ed339b589994df82e1f528be774aff341", hex.EncodeToString(idem.Request{}.Fingerprint()))
	assert.Len(t, req.Fingerprint(), idem.FingerprintSize)
}

// Без префикса длины соседние поля склеивались бы. Пары с нулевыми байтами
// внутри поля ловят и постоянный разделитель вместо длины.
func TestFingerprint_FieldBoundariesDoNotCollide(t *testing.T) {
	t.Parallel()

	nul := strings.Repeat("\x00", 8)
	pairs := [][2]idem.Request{
		{{Path: "/ab", RawQuery: "c"}, {Path: "/a", RawQuery: "bc"}},
		{{RawQuery: "ab", Body: []byte("c")}, {RawQuery: "a", Body: []byte("bc")}},
		{{Method: "POST", Path: "/x"}, {Method: "POS", Path: "T/x"}},
		{{Operation: "a.b", Method: "c"}, {Operation: "a.bc"}},
		{{Path: "/ab" + nul + "c", RawQuery: "d"}, {Path: "/ab", RawQuery: "c" + nul + "d"}},
		{{RawQuery: "x" + nul, Body: []byte("y")}, {RawQuery: "x", Body: []byte(nul + "y")}},
	}
	for _, pair := range pairs {
		assert.NotEqual(t, pair[0].Fingerprint(), pair[1].Fingerprint(), "%+v и %+v", pair[0], pair[1])
	}
}

// Каждое поле отпечатка его меняет; область, ключ и заголовков в нём нет.
func TestFingerprint_CoversRequestNotScopeOrKey(t *testing.T) {
	t.Parallel()

	base := validRequest(t)
	for _, tc := range []struct {
		what   string
		change func(r *idem.Request)
	}{
		{"операция", func(r *idem.Request) { r.Operation = opUpdate }},
		{"метод", func(r *idem.Request) { r.Method = http.MethodPatch }},
		{"путь", func(r *idem.Request) { r.Path = "/orders/" }},
		{"путь в записи", func(r *idem.Request) { r.Path = "/%6Frders" }},
		{"строка запроса", func(r *idem.Request) { r.RawQuery = "source=app" }},
		{"тело", func(r *idem.Request) { r.Body = []byte(`{"product": "a"}`) }},
		{"пустое тело", func(r *idem.Request) { r.Body = nil }},
	} {
		other := base
		tc.change(&other)
		assert.NotEqual(t, base.Fingerprint(), other.Fingerprint(), tc.what)
	}

	same := base
	same.Scope = idem.Scope{Realm: "staff", Subject: "other"}
	same.Key = mustKey(t, "k-2")
	same.Body = []byte(`{"product":"a"}`)
	assert.Equal(t, base.Fingerprint(), same.Fingerprint(), "область и ключ не входят в отпечаток")
}

func TestCheckRequest(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	require.NoError(t, cfg.CheckRequest(validRequest(t)))

	patch := validRequest(t)
	patch.Method = http.MethodPatch
	require.NoError(t, cfg.CheckRequest(patch), "PATCH")

	edge := validRequest(t)
	edge.Scope = idem.Scope{Realm: strings.Repeat("a", idem.MaxRealmLen), Subject: strings.Repeat("ж", idem.MaxSubjectLen/2)}
	require.NoError(t, cfg.CheckRequest(edge), "реалм и субъект ровно на потолке")

	for _, tc := range []struct {
		what   string
		change func(r *idem.Request)
		want   error
	}{
		{"пустая область", func(r *idem.Request) { r.Scope = idem.Scope{} }, idem.ErrInvalidScope},
		{"пустой реалм", func(r *idem.Request) { r.Scope.Realm = "" }, idem.ErrInvalidScope},
		{"реалм длиннее потолка", func(r *idem.Request) { r.Scope.Realm = strings.Repeat("a", idem.MaxRealmLen+1) }, idem.ErrInvalidScope},
		{"реалм с заглавной", func(r *idem.Request) { r.Scope.Realm = "Customers" }, idem.ErrInvalidScope},
		{"реалм с точкой", func(r *idem.Request) { r.Scope.Realm = "a.b" }, idem.ErrInvalidScope},
		{"пустой субъект", func(r *idem.Request) { r.Scope.Subject = "" }, idem.ErrInvalidScope},
		{"субъект длиннее потолка", func(r *idem.Request) { r.Scope.Subject = strings.Repeat("s", idem.MaxSubjectLen+1) }, idem.ErrInvalidScope},
		{"субъект с NUL", func(r *idem.Request) { r.Scope.Subject = "a\x00" }, idem.ErrInvalidScope},
		{"субъект с DEL", func(r *idem.Request) { r.Scope.Subject = "a\x7f" }, idem.ErrInvalidScope},
		{"субъект с C1", func(r *idem.Request) { r.Scope.Subject = "a\u0085" }, idem.ErrInvalidScope},
		{"субъект не UTF-8", func(r *idem.Request) { r.Scope.Subject = "a\xff" }, idem.ErrInvalidScope},
		{"операция вне набора", func(r *idem.Request) { r.Operation = "orders.delete" }, idem.ErrInvalidRequest},
		{"пустая операция", func(r *idem.Request) { r.Operation = "" }, idem.ErrInvalidRequest},
		{"ключ не разобран", func(r *idem.Request) { r.Key = idem.Key{} }, idem.ErrInvalidRequest},
		{"метод GET", func(r *idem.Request) { r.Method = http.MethodGet }, idem.ErrInvalidRequest},
		{"метод PUT", func(r *idem.Request) { r.Method = http.MethodPut }, idem.ErrInvalidRequest},
		{"метод DELETE", func(r *idem.Request) { r.Method = http.MethodDelete }, idem.ErrInvalidRequest},
		{"метод post строчными", func(r *idem.Request) { r.Method = "post" }, idem.ErrInvalidRequest},
	} {
		req := validRequest(t)
		tc.change(&req)
		assert.ErrorIs(t, cfg.CheckRequest(req), tc.want, tc.what)
	}
}
