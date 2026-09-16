package inbox_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxhttp"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel несёт класс или отказ от него с доводом
// (ADR-0007). Двойник в allow: его ошибки — поломка стенда либо обёртка
// sentinel модуля.
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "inboxtest")
}

// Классы поимённо — таблица решения 13 ADR-0012: сдвиг любого меняет ответ
// отправителю и виден в диффе. Префикс пакета держит KindError разных модулей
// неравными через errors.Is; рукописно, пока страж kit не выпущен тегом.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	table := []struct {
		name   string
		err    error
		kind   errs.Kind
		prefix string
	}{
		{"ErrNotAuthentic", inbox.ErrNotAuthentic, errs.KindIncorrectInput, "inbox: "},
		{"ErrMalformed", inbox.ErrMalformed, errs.KindUnavailable, "inbox: "},
		{"ErrUnknownType", inbox.ErrUnknownType, errs.KindUnavailable, "inbox: "},
		{"ErrInFlight", inbox.ErrInFlight, errs.KindConflict, "inbox: "},
		{"ErrTooLarge", inbox.ErrTooLarge, errs.KindPayloadTooLarge, "inbox: "},
		{"ErrUnavailable", inbox.ErrUnavailable, errs.KindUnavailable, "inbox: "},
		{"ErrUnknownSource", inbox.ErrUnknownSource, errs.KindUnknown, "inbox: "},
		{"inboxhttp.ErrBodyUnreadable", inboxhttp.ErrBodyUnreadable, errs.KindIncorrectInput, "inboxhttp: "},
		{"inboxtest.ErrNoHandler", inboxtest.ErrNoHandler, errs.KindUnknown, "inboxtest: "},
		{"inboxtest.ErrSchemaCheck", inboxtest.ErrSchemaCheck, errs.KindUnavailable, "inbox: "},
	}
	listed := make([]string, 0, len(table))
	for _, tc := range table {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), tc.prefix), "текст %s без префикса %q: %q", tc.name, tc.prefix, tc.err.Error())
		listed = append(listed, tc.name)
	}

	declared := slices.Concat(
		declaredSentinels(t, ".", ""),
		declaredSentinels(t, "inboxhttp", "inboxhttp."),
		declaredSentinels(t, "inboxtest", "inboxtest."),
	)
	slices.Sort(declared)
	slices.Sort(listed)
	assert.Equal(t, declared, listed, "sentinel, забытая в таблице, не проверяется ни на класс, ни на префикс")

	// Тексты ядра разные: KindError одного класса равны по тексту.
	core := table[:7]
	for i, a := range core {
		for _, b := range core[i+1:] {
			assert.NotErrorIs(t, a.err, b.err, "%s совпала с %s через errors.Is", a.name, b.name)
		}
	}
}

// declaredSentinels — экспортируемые package-level var Err* каталога.
func declaredSentinels(t *testing.T, dir, qualifier string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
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

// Сбой порта доходит до вызывающего с классом 503 и причиной в цепочке: класс
// несёт обёртка ядра. Хранилище и верификатор — голые заглушки: двойник
// заворачивает сбой сам, и снятой обёртки ядра страж бы не увидел.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	receive := func(store inbox.Store, verifier inbox.Verifier) error {
		svc := inbox.NewService(store, inboxtest.NewObserver(), testConfig(verifier))
		svc.SetClock(func() time.Time { return start })
		_, err := svc.Receive(t.Context(), billing, signed("evt_port", typePaid, nil))
		return err
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Accept", receive(bareStore{acceptErr: down}, stubVerifier())},
		{"обработчик со слагом", receive(bareStore{acceptErr: errs.Conflict("seat-taken")}, stubVerifier())},
		{"Verify", receive(bareStore{}, verifierFunc(func(context.Context, inbox.Request) (inbox.Event, error) {
			return inbox.Event{}, down
		}))},
		{"Purge", func() error {
			svc := inbox.NewService(bareStore{purgeErr: down}, inboxtest.NewObserver(), testConfig(stubVerifier()))
			_, err := svc.Purge(t.Context())
			return err
		}()},
	} {
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(tc.err), tc.name)
		require.ErrorIs(t, tc.err, inbox.ErrUnavailable, tc.name)
		assert.NotEqual(t, inbox.ErrUnavailable.Error(), tc.err.Error(), "%s: причина пропала из текста", tc.name)
	}
}

// Исход хранилища вне контракта Accept — не 200: иначе ошибка адаптера
// превратилась бы в подтверждение и потеряла событие.
func TestReceive_StoreOutcomeOutsideContract(t *testing.T) {
	t.Parallel()

	for _, outcome := range []inbox.Outcome{inbox.OutcomeIgnored, inbox.OutcomeNotAuthentic, inbox.OutcomeError, ""} {
		svc := inbox.NewService(bareStore{outcome: outcome}, inboxtest.NewObserver(), testConfig(stubVerifier()))
		svc.SetClock(func() time.Time { return start })
		receipt, err := svc.Receive(t.Context(), billing, signed("evt_contract", typePaid, nil))
		require.ErrorIs(t, err, inbox.ErrUnavailable, "исход %q", outcome)
		assert.Equal(t, inbox.OutcomeError, receipt.Outcome, "исход %q", outcome)
	}
}

// bareStore — inbox.Store, который отвечает заданным без обёрток.
type bareStore struct {
	outcome   inbox.Outcome
	acceptErr error
	purgeErr  error
}

func (s bareStore) Accept(context.Context, inbox.Event, time.Time) (inbox.Outcome, error) {
	if s.acceptErr != nil {
		return "", s.acceptErr
	}
	return s.outcome, nil
}

func (s bareStore) Sources() []inbox.SourceName { return []inbox.SourceName{billing} }

func (s bareStore) Purge(context.Context, time.Time, time.Time, int) (int, error) {
	return 0, s.purgeErr
}
