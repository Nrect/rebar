package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Каждая экспортируемая sentinel несёт класс или отказ от него с доводом
// (ADR-0007). Двойник в allow: своего класса у его sentinel нет, класс
// приходит обёрткой.
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "ledgertest")
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и виден в диффе.
// Префикс пакета держит KindError разных модулей неравными через errors.Is;
// рукописно, пока страж kit не выпущен тегом.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	table := []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"ErrInvalidRequest", ledger.ErrInvalidRequest, errs.KindUnknown},
		{"ErrInvalidKey", ledger.ErrInvalidKey, errs.KindUnknown},
		{"ErrUnknownKind", ledger.ErrUnknownKind, errs.KindUnknown},
		{"ErrKeyReused", ledger.ErrKeyReused, errs.KindConflict},
		{"ErrInsufficientFunds", ledger.ErrInsufficientFunds, errs.KindConflict},
		{"ErrEntryNotFound", ledger.ErrEntryNotFound, errs.KindNotFound},
		{"ErrNotReversible", ledger.ErrNotReversible, errs.KindConflict},
		{"ErrAlreadyReversed", ledger.ErrAlreadyReversed, errs.KindConflict},
		{"ErrUnavailable", ledger.ErrUnavailable, errs.KindUnavailable},
		{"ledgertest.ErrTxDone", ledgertest.ErrTxDone, errs.KindUnavailable},
		{"ledgertest.ErrUnknownBook", ledgertest.ErrUnknownBook, errs.KindUnknown},
	}
	listed := make([]string, 0, len(table))
	for _, tc := range table {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), "ledger: "), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
		listed = append(listed, tc.name)
	}

	declared := append(declaredSentinels(t, ".", ""), declaredSentinels(t, "ledgertest", "ledgertest.")...)
	slices.Sort(declared)
	slices.Sort(listed)
	assert.Equal(t, declared, listed, "sentinel, забытая в таблице, не проверяется ни на класс, ни на префикс")
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
// несёт обёртка ядра. Хранилище — голая заглушка: ledgertest.MemStore
// заворачивает сбой сам, и снятой обёртки ядра страж бы не увидел.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	target := ledger.Entry{ID: uuid.New(), Kind: kindTopup, AmountMinor: 100}
	cfg := testConfig(t)
	post := func(svc *ledger.Service) error {
		_, err := svc.Post(t.Context(), ledger.PostRequest{
			Account: uuid.New(), Kind: kindTopup, AmountMinor: 100, Reference: "payment:1", IdempotencyKey: "k",
		})
		return err
	}
	reverse := func(svc *ledger.Service) error {
		_, err := svc.Reverse(t.Context(), ledger.ReverseRequest{
			Account: uuid.New(), EntryID: target.ID, By: byOperator, Reason: "mistake", Actor: "staff:1", IdempotencyKey: "k",
		})
		return err
	}
	balance := func(svc *ledger.Service) error {
		_, err := svc.Balance(t.Context(), uuid.New())
		return err
	}
	verify := func(svc *ledger.Service) error {
		_, err := svc.Verify(t.Context(), uuid.New(), ledger.Position{}, 10)
		return err
	}
	reconcile := func(svc *ledger.Service) error {
		rec := ledger.NewReconciler(svc, ledgertest.NewObserver(), ledger.ReconcileConfig{Accounts: 10, Page: 10})
		_, err := rec.Run(t.Context())
		return err
	}

	for _, tc := range []struct {
		failing string
		call    func(*ledger.Service) error
	}{
		{"Post", post}, {"EntryByKey", post}, {"Insert", post},
		{"EntryByID", reverse}, {"ReversalOf", reverse},
		{"Account", balance}, {"Entries", verify},
		{"Accounts", reconcile}, {"Account", reconcile}, {"Entries", reconcile},
	} {
		svc := ledger.NewService(bareStore{failing: tc.failing, err: down, target: target}, cfg)
		err := tc.call(svc)
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), tc.failing)
		require.ErrorIs(t, err, ledger.ErrUnavailable, tc.failing)
		require.ErrorIs(t, err, down, "%s: причина в цепочке", tc.failing)
	}
}

// Отказ схемы при вставке (контракт AccountTx.Insert) остаётся своим
// классом: 409 на нехватке остатка не должен стать 503. Уже завёрнутый сбой
// не заворачивается дважды.
func TestStoreRefusalsKeepTheirClass(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	post := func(insertErr error) error {
		svc := ledger.NewService(bareStore{failing: "Insert", err: insertErr}, cfg)
		_, err := svc.Post(t.Context(), ledger.PostRequest{
			Account: uuid.New(), Kind: kindTopup, AmountMinor: 100, Reference: "payment:1", IdempotencyKey: "k",
		})
		return err
	}
	for _, refusal := range []error{
		ledger.ErrInsufficientFunds, ledger.ErrAlreadyReversed, ledger.ErrNotReversible,
		ledger.ErrEntryNotFound, ledger.ErrUnknownKind, ledger.ErrInvalidRequest,
	} {
		err := post(fmt.Errorf("%w: adapter detail", refusal))
		require.ErrorIs(t, err, refusal)
		assert.Equal(t, errs.KindOf(refusal), errs.KindOf(err), "класс %v", refusal)
		require.NotErrorIs(t, err, ledger.ErrUnavailable, "%v под ErrUnavailable", refusal)
	}

	err := post(fmt.Errorf("%w: adapter: serialization failure", ledger.ErrUnavailable))
	assert.Equal(t, 1, strings.Count(err.Error(), ledger.ErrUnavailable.Error()), "сбой завёрнут дважды: %q", err.Error())
}

// bareStore — ledger.Store, у которого названный метод отвечает голой ошибкой;
// остальные отвечают пусто, Accounts отдаёт один счёт, а EntryByID находит
// target.
type bareStore struct {
	failing string
	err     error
	target  ledger.Entry
}

func (s bareStore) fail(method string) error {
	if s.failing == method {
		return s.err
	}
	return nil
}

func (s bareStore) Post(_ context.Context, _ string, _ uuid.UUID, fn func(ledger.AccountTx, ledger.Account) error) error {
	if err := s.fail("Post"); err != nil {
		return err
	}
	return fn(bareAccount(s), ledger.Account{})
}

func (s bareStore) Account(context.Context, string, uuid.UUID) (ledger.Account, error) {
	return ledger.Account{}, s.fail("Account")
}

func (s bareStore) Entries(context.Context, string, uuid.UUID, int64, int) ([]ledger.Entry, error) {
	return nil, s.fail("Entries")
}

func (s bareStore) Accounts(context.Context, string, uuid.UUID, int) ([]uuid.UUID, error) {
	return []uuid.UUID{s.target.ID}, s.fail("Accounts")
}

type bareAccount bareStore

func (a bareAccount) EntryByKey(context.Context, string) (ledger.Entry, bool, error) {
	return ledger.Entry{}, false, bareStore(a).fail("EntryByKey")
}

func (a bareAccount) EntryByID(context.Context, uuid.UUID) (ledger.Entry, bool, error) {
	return a.target, true, bareStore(a).fail("EntryByID")
}

func (a bareAccount) ReversalOf(context.Context, uuid.UUID) (ledger.Entry, bool, error) {
	return ledger.Entry{}, false, bareStore(a).fail("ReversalOf")
}

func (a bareAccount) Insert(context.Context, ledger.Entry) error { return bareStore(a).fail("Insert") }
