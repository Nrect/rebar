package idem_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

func TestCheckResponse_Records(t *testing.T) {
	t.Parallel()

	cfg := testConfig() // потолок 64 байта
	for _, tc := range []struct {
		what string
		resp idem.Response
	}{
		{"200 с телом", idem.Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{}`)}},
		{"201 с Location", idem.Response{Status: http.StatusCreated, ContentType: "application/json", Location: "/orders/1", Body: []byte(`{}`)}},
		{"204 без тела", idem.Response{Status: http.StatusNoContent}},
		{"303 с Location", idem.Response{Status: http.StatusSeeOther, Location: "/orders/1"}},
		{"отказ 409 — тоже результат", idem.Response{Status: http.StatusConflict, ContentType: "application/json", Body: []byte(`{"slug":"closed"}`)}},
		{"499 — последний записываемый", idem.Response{Status: 499}},
		{"ровно потолок", idem.Response{Status: http.StatusOK, ContentType: "text/plain", Location: "/x", Body: []byte(strings.Repeat("b", 64-len("text/plain")-len("/x")))}},
		{"заголовок с пробелом внутри", idem.Response{Status: http.StatusOK, ContentType: "text/plain; charset=utf-8", Body: []byte("ok")}},
		{"Location с процентной записью", idem.Response{Status: http.StatusCreated, Location: "/orders/%D0%B7"}},
	} {
		assert.NoError(t, cfg.CheckResponse(tc.resp), tc.what)
	}
}

func TestCheckResponse_Refuses(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	for _, tc := range []struct {
		what string
		resp idem.Response
		want error
	}{
		{"нулевой ответ", idem.Response{}, idem.ErrNotRecordable},
		{"199", idem.Response{Status: 199}, idem.ErrNotRecordable},
		{"100 Continue", idem.Response{Status: http.StatusContinue}, idem.ErrNotRecordable},
		{"500", idem.Response{Status: http.StatusInternalServerError}, idem.ErrNotRecordable},
		{"503", idem.Response{Status: http.StatusServiceUnavailable}, idem.ErrNotRecordable},
		{"отрицательный статус", idem.Response{Status: -200}, idem.ErrNotRecordable},
		{"тело без Content-Type", idem.Response{Status: http.StatusOK, Body: []byte("<b>x</b>")}, idem.ErrNotRecordable},
		{"тело у 204", idem.Response{Status: http.StatusNoContent, ContentType: "text/plain", Body: []byte("x")}, idem.ErrNotRecordable},
		{"тело у 304", idem.Response{Status: http.StatusNotModified, ContentType: "text/plain", Body: []byte("x")}, idem.ErrNotRecordable},
		{"CRLF в Location", idem.Response{Status: http.StatusCreated, Location: "/a\r\nSet-Cookie: s=1"}, idem.ErrNotRecordable},
		{"перевод строки в Content-Type", idem.Response{Status: http.StatusOK, ContentType: "text/plain\n"}, idem.ErrNotRecordable},
		{"табуляция в Content-Type", idem.Response{Status: http.StatusOK, ContentType: "text/plain;\tq=1"}, idem.ErrNotRecordable},
		{"DEL в Location", idem.Response{Status: http.StatusCreated, Location: "/a\x7f"}, idem.ErrNotRecordable},
		{"не ASCII в Location", idem.Response{Status: http.StatusCreated, Location: "/заказы"}, idem.ErrNotRecordable},
		{"пробел в начале заголовка", idem.Response{Status: http.StatusOK, ContentType: " text/plain"}, idem.ErrNotRecordable},
		{"пробел в конце заголовка", idem.Response{Status: http.StatusCreated, Location: "/a "}, idem.ErrNotRecordable},
		{"тело на байт больше потолка", idem.Response{Status: http.StatusOK, ContentType: "text/plain", Body: []byte(strings.Repeat("b", 65-len("text/plain")))}, idem.ErrResponseTooLarge},
		{"Location на байт больше", idem.Response{Status: http.StatusCreated, Location: "/" + strings.Repeat("l", 64)}, idem.ErrResponseTooLarge},
		{"Content-Type на байт больше", idem.Response{Status: http.StatusOK, ContentType: strings.Repeat("t", 65)}, idem.ErrResponseTooLarge},
	} {
		assert.ErrorIs(t, cfg.CheckResponse(tc.resp), tc.want, tc.what)
	}
}

// Текст отказа называет настоящие величины: статус, размер и потолок. Длины
// разные, иначе сложение и вычитание в тексте неразличимы.
func TestCheckResponse_ErrorNamesTheNumbers(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	err := cfg.CheckResponse(idem.Response{Status: http.StatusOK, ContentType: "text/plain", Location: "/abc", Body: []byte(strings.Repeat("b", 90))})
	require.ErrorIs(t, err, idem.ErrResponseTooLarge)
	assert.Contains(t, err.Error(), "104 bytes, max is 64")

	err = cfg.CheckResponse(idem.Response{Status: http.StatusBadGateway})
	require.ErrorIs(t, err, idem.ErrNotRecordable)
	assert.Contains(t, err.Error(), "status 502 is outside 200..499")
}

func TestReplay(t *testing.T) {
	t.Parallel()

	req := validRequest(t)
	recorded := idem.Response{Status: http.StatusCreated, ContentType: "application/json", Location: "/orders/1", Body: []byte(`{"order":1}`)}
	rec := idem.Record{Fingerprint: req.Fingerprint(), Response: recorded}

	res, err := idem.Replay(req, rec)
	require.NoError(t, err)
	assert.True(t, res.Replayed)
	assert.Equal(t, recorded, res.Response)

	res.Response.Body[0] = 'X'
	assert.Equal(t, byte('{'), rec.Response.Body[0], "повтор отдаёт копию тела, а не память записи")

	other := req
	other.Body = []byte(`{"product":"b"}`)
	for _, tc := range []struct {
		what string
		req  idem.Request
		rec  idem.Record
	}{
		{"другой запрос", other, rec},
		{"пустой отпечаток в записи", req, idem.Record{Response: recorded}},
		{"усечённый отпечаток", req, idem.Record{Fingerprint: req.Fingerprint()[:31], Response: recorded}},
		{"отпечаток с лишним байтом", req, idem.Record{Fingerprint: append(req.Fingerprint(), 0), Response: recorded}},
		{"нулевой отпечаток той длины", req, idem.Record{Fingerprint: make([]byte, idem.FingerprintSize), Response: recorded}},
	} {
		res, err := idem.Replay(tc.req, tc.rec)
		assert.Equal(t, idem.Result{}, res, tc.what)
		assert.ErrorIs(t, err, idem.ErrKeyReused, tc.what)
	}
}
