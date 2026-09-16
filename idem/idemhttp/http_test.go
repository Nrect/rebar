package idemhttp_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemhttp"
)

var scope = idem.Scope{Realm: "customers", Subject: "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"}

func TestKey(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/orders", http.NoBody)
	_, err := idemhttp.Key(r)
	require.ErrorIs(t, err, idem.ErrKeyMissing, "нет заголовка")

	r.Header.Set("idempotency-key", `"a1f3"`)
	key, err := idemhttp.Key(r)
	require.NoError(t, err, "имя заголовка без учёта регистра")
	assert.Equal(t, "a1f3", key.String())

	r.Header.Add(idemhttp.HeaderKey, "a1f3")
	_, err = idemhttp.Key(r)
	require.ErrorIs(t, err, idem.ErrKeyInvalid, "две строки поля")

	r.Header.Set(idemhttp.HeaderKey, "")
	_, err = idemhttp.Key(r)
	require.ErrorIs(t, err, idem.ErrKeyInvalid, "пустое значение — не отсутствие")
}

// Путь и строка запроса — как пришли: другая запись того же пути даёт другой
// отпечаток, а не чужой ответ.
func TestNewRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{"product":"tea"}`)
	r := httptest.NewRequest(http.MethodPatch, "/orders/%41b?x=1&y=%20", strings.NewReader("не читается"))
	r.Header.Set(idemhttp.HeaderKey, "k-1")

	req, err := idemhttp.NewRequest(r, scope, "orders.update", body)
	require.NoError(t, err)
	assert.Equal(t, idem.Request{
		Scope: scope, Operation: "orders.update", Key: mustKey(t, "k-1"),
		Method: http.MethodPatch, Path: "/orders/%41b", RawQuery: "x=1&y=%20", Body: body,
	}, req)

	decoded := httptest.NewRequest(http.MethodPatch, "/orders/Ab?x=1&y=%20", http.NoBody)
	decoded.Header.Set(idemhttp.HeaderKey, "k-1")
	other, err := idemhttp.NewRequest(decoded, scope, "orders.update", body)
	require.NoError(t, err)
	assert.NotEqual(t, req.Fingerprint(), other.Fingerprint(), "%41b и Ab — разные запросы для отпечатка")

	missing := httptest.NewRequest(http.MethodPost, "/orders", http.NoBody)
	req, err = idemhttp.NewRequest(missing, scope, "orders.create", body)
	require.ErrorIs(t, err, idem.ErrKeyMissing)
	assert.Equal(t, idem.Request{}, req)
}

func TestJSON(t *testing.T) {
	t.Parallel()

	resp, err := idemhttp.JSON(http.StatusCreated, map[string]any{"order": 1, "product": "tea"})
	require.NoError(t, err)
	assert.Equal(t, idem.Response{
		Status: http.StatusCreated, ContentType: "application/json", Body: []byte(`{"order":1,"product":"tea"}`),
	}, resp)

	_, err = idemhttp.JSON(http.StatusOK, map[string]any{"bad": make(chan int)})
	require.Error(t, err, "ошибка кодирования — ошибка op")
}

func TestWrite(t *testing.T) {
	t.Parallel()

	resp := idem.Response{
		Status: http.StatusCreated, ContentType: "application/json", Location: "/orders/1", Body: []byte(`{"order":1}`),
	}
	executed := httptest.NewRecorder()
	idemhttp.Write(executed, idem.Result{Response: resp})
	assert.Equal(t, http.StatusCreated, executed.Code)
	assert.Equal(t, `{"order":1}`, executed.Body.String())
	assert.Equal(t, http.Header{"Content-Type": {"application/json"}, "Location": {"/orders/1"}}, executed.Header(),
		"исполненный ответ без Idempotent-Replayed и без чужих заголовков")

	replayed := httptest.NewRecorder()
	idemhttp.Write(replayed, idem.Result{Response: resp, Replayed: true})
	assert.Equal(t, http.StatusCreated, replayed.Code)
	assert.Equal(t, `{"order":1}`, replayed.Body.String())
	assert.Equal(t, "true", replayed.Header().Get(idemhttp.HeaderReplayed))

	empty := httptest.NewRecorder()
	idemhttp.Write(empty, idem.Result{Response: idem.Response{Status: http.StatusNoContent}, Replayed: true})
	assert.Equal(t, http.StatusNoContent, empty.Code)
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range empty.Header() {
			if !yield(k) {
				return
			}
		}
	})
	assert.Equal(t, []string{idemhttp.HeaderReplayed}, keys, "пустые Content-Type и Location не ставятся")
}

func mustKey(t *testing.T, raw string) idem.Key {
	t.Helper()
	key, err := idem.ParseKey(raw)
	require.NoError(t, err)
	return key
}
