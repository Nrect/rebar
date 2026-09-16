package idem_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с
// доводом (ADR-0007).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".")
}

// Классы поимённо — таблица ADR-0012, решение 13: сдвиг любого меняет ответ
// потребителю и виден в диффе. Префикс пакета держит KindError разных модулей
// неравными через errors.Is; рукописно, пока страж kit не выпущен тегом.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	table := []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"ErrKeyMissing", idem.ErrKeyMissing, errs.KindIncorrectInput},
		{"ErrKeyInvalid", idem.ErrKeyInvalid, errs.KindIncorrectInput},
		{"ErrKeyReused", idem.ErrKeyReused, errs.KindConflict},
		{"ErrInFlight", idem.ErrInFlight, errs.KindConflict},
		{"ErrUnavailable", idem.ErrUnavailable, errs.KindUnavailable},
		{"ErrNotRecordable", idem.ErrNotRecordable, errs.KindUnknown},
		{"ErrResponseTooLarge", idem.ErrResponseTooLarge, errs.KindUnknown},
		{"ErrInvalidScope", idem.ErrInvalidScope, errs.KindUnknown},
		{"ErrInvalidRequest", idem.ErrInvalidRequest, errs.KindUnknown},
	}
	listed := make([]string, 0, len(table))
	for _, tc := range table {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), "idem: "), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
		listed = append(listed, tc.name)
	}

	declared := make([]string, 0, len(table))
	for _, dir := range []string{".", "idemtest", "idemhttp"} {
		declared = append(declared, declaredSentinels(t, dir)...)
	}
	slices.Sort(declared)
	slices.Sort(listed)
	assert.Equal(t, declared, listed, "sentinel, забытая в таблице, не проверяется ни на класс, ни на префикс")
}

// declaredSentinels — экспортируемые package-level var Err* каталога; у
// подпакетов с префиксом каталога, чтобы не совпасть с ядром.
func declaredSentinels(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	qualifier := ""
	if dir != "." {
		qualifier = dir + "."
	}
	var names []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				require.True(t, isValue, "%s: объявление var без значения", name)
				for _, ident := range value.Names {
					if ident.IsExported() && strings.HasPrefix(ident.Name, "Err") {
						names = append(names, qualifier+ident.Name)
					}
				}
			}
		}
	}
	return names
}

// Два 409 различимы: у занятого ключа Retry-After через структурный контракт
// httperr, у переиспользованного нет (ADR-0012, решение 12). Проверено ответом
// настоящего httperr, а не только методом.
func TestInFlight_RetryAfterReachesResponse(t *testing.T) {
	t.Parallel()

	err := idem.InFlight()
	require.ErrorIs(t, err, idem.ErrInFlight)
	require.NotErrorIs(t, err, idem.ErrKeyReused)
	assert.Equal(t, idem.ErrInFlight.Error(), err.Error())
	assert.Equal(t, errs.KindConflict, errs.KindOf(err))

	respond := httperr.New(httperr.Config{Logger: slog.New(slog.DiscardHandler)})
	for _, tc := range []struct {
		err        error
		status     int
		retryAfter string
	}{
		{err, http.StatusConflict, "1"},
		{fmt.Errorf("%w: store: lock is taken", err), http.StatusConflict, "1"},
		{fmt.Errorf("%w: recorded fingerprint differs", idem.ErrKeyReused), http.StatusConflict, ""},
		{idem.ErrKeyMissing, http.StatusBadRequest, ""},
		{fmt.Errorf("%w: store: commit: connection reset", idem.ErrUnavailable), http.StatusServiceUnavailable, ""},
		{idem.ErrNotRecordable, http.StatusInternalServerError, ""},
	} {
		rec := httptest.NewRecorder()
		respond.Write(t.Context(), rec, tc.err)
		assert.Equal(t, tc.status, rec.Code, "статус на %v", tc.err)
		assert.Equal(t, tc.retryAfter, rec.Header().Get("Retry-After"), "Retry-After на %v", tc.err)
	}
}

// Сбой порта доходит до вызывающего с классом 503 и причиной в цепочке: класс
// несёт обёртка ядра. Хранилище — голая заглушка: idemtest.MemStore
// заворачивает сбой сам, и снятой обёртки ядра страж бы не увидел.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	purger := idem.NewPurger(&barePruner{err: down}, testConfig())
	_, err := purger.Run(t.Context())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	require.ErrorIs(t, err, idem.ErrUnavailable)
	require.ErrorIs(t, err, down, "причина в цепочке")

	wrapped := fmt.Errorf("%w: adapter: purge: %w", idem.ErrUnavailable, down)
	_, err = idem.NewPurger(&barePruner{err: wrapped}, testConfig()).Run(t.Context())
	assert.Equal(t, 1, strings.Count(err.Error(), idem.ErrUnavailable.Error()), "сбой завёрнут дважды: %q", err.Error())
}

// Тексты ошибок не цитируют вход: ключ и заголовок пишет клиент, субъект —
// идентификатор человека, заголовки ответа строит ручка из данных.
func TestErrors_DoNotQuoteInput(t *testing.T) {
	t.Parallel()

	const marker = "Marker7fq2"
	cfg := testConfig()
	failures := make([]error, 0, 11)

	for _, raw := range []string{marker + " x", `"` + marker, marker + ";a=b", `"` + marker + `\"`} {
		_, err := idem.ParseKey(raw)
		failures = append(failures, err)
	}
	_, err := idem.ParseKey(marker, marker)
	failures = append(failures, err)

	req := validRequest(t)
	req.Scope.Subject = marker + "\x00"
	failures = append(failures, cfg.CheckRequest(req))
	req = validRequest(t)
	req.Scope.Realm = marker
	failures = append(failures, cfg.CheckRequest(req))

	for _, resp := range []idem.Response{
		{Status: http.StatusOK, ContentType: "text/plain", Location: marker + "\r\n"},
		{Status: http.StatusOK, ContentType: marker + "\n"},
		{Status: http.StatusOK, ContentType: "text/plain", Body: []byte(strings.Repeat(marker, 100))},
	} {
		failures = append(failures, cfg.CheckResponse(resp))
	}
	_, err = idem.Replay(validRequest(t), idem.Record{Fingerprint: []byte(marker), Response: idem.Response{Body: []byte(marker)}})
	failures = append(failures, err)

	for i, failure := range failures {
		require.Error(t, failure, "случай %d обязан быть отказом", i)
		assert.NotContains(t, failure.Error(), marker, "случай %d: текст ошибки цитирует вход", i)
	}
}

// barePruner — idem.Pruner, отвечающий голой ошибкой.
type barePruner struct{ err error }

func (p *barePruner) Purge(context.Context, time.Time, int) (int, error) { return 0, p.err }
