package paymentotel_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
)

// Набор result закрыт: на этих строках стоят алерты потребителя, и смена любой
// из них ломающая (VERSIONING).
func TestAllResults_IsClosedSet(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []paymentotel.Result{"ok", "rejected", "error"}, paymentotel.AllResults)
}

// Набор type закрыт по той же причине; строки держатся литералами.
func TestAllCallTypes_IsClosedSet(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []paymentotel.CallType{
		"create_payment", "parse_webhook", "get_payment", "capture", "cancel", "refund",
	}, paymentotel.AllCallTypes)
}

// Новый метод порта не останется без метки: у каждого метода, кроме Name, есть
// свой type — его имя в snake_case.
func TestAllCallTypes_CoverThePort(t *testing.T) {
	t.Parallel()
	port := reflect.TypeFor[payment.Provider]()
	want := make([]paymentotel.CallType, 0, port.NumMethod())
	for i := range port.NumMethod() {
		if name := port.Method(i).Name; name != "Name" {
			want = append(want, paymentotel.CallType(snake(name)))
		}
	}

	assert.ElementsMatch(t, want, paymentotel.AllCallTypes)
}

// Все пары закрытых наборов достижимы, и ни одна точка счётчика не несёт метки
// вне них: страж ловит исход, добавленный в код и забытый в All*.
func TestWrap_EveryPointIsFromClosedSets(t *testing.T) {
	t.Parallel()
	stub := newStub(nil)
	p, reader := wrap(t, stub)

	for _, err := range []error{nil, payment.ErrProviderRejected, payment.ErrUnavailable} {
		stub.err = err
		for _, pc := range everyCall() {
			_ = pc.do(context.Background(), p)
		}
	}

	points := callPoints(t, collect(t, reader))
	assert.Len(t, points, len(paymentotel.AllCallTypes)*len(paymentotel.AllResults), "все пары достижимы")
	for _, dp := range points {
		require.Equal(t, 2, dp.Attributes.Len(), "меток ровно две")
		typ, ok := dp.Attributes.Value("type")
		require.True(t, ok, "у точки нет метки type")
		result, ok := dp.Attributes.Value("result")
		require.True(t, ok, "у точки нет метки result")
		assert.Contains(t, paymentotel.AllCallTypes, paymentotel.CallType(typ.AsString()))
		assert.Contains(t, paymentotel.AllResults, paymentotel.Result(result.AsString()))
	}
}

// У каждого рода из payment.AllDriftKinds свой ряд, и ни одного ряда с родом
// вне набора: метка из внешнего мира в метрику не попадает.
func TestGauges_EveryDriftKindHasSeries(t *testing.T) {
	t.Parallel()
	g, reader := newGauges(t)
	g.Set(paymentotel.Snapshot{Drift: drifts("made_up_by_store", 1)})

	got := driftByKind(t, collect(t, reader))
	delete(got, noKind)
	kinds := make([]string, 0, len(got))
	for kind := range got {
		kinds = append(kinds, kind)
	}
	want := make([]string, 0, len(payment.AllDriftKinds))
	for _, kind := range payment.AllDriftKinds {
		want = append(want, string(kind))
	}

	assert.ElementsMatch(t, want, kinds)
}

// snake — CreatePayment → create_payment.
func snake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
