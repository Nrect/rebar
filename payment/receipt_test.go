package payment_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
)

func TestCheckReceipt_Rules(t *testing.T) {
	t.Parallel()

	good := func() *payment.Receipt {
		return &payment.Receipt{
			Customer:  payment.Customer{Email: "buyer@shop.ru"},
			TaxSystem: "1",
			Items: []payment.ReceiptItem{
				{Description: "Книга", AmountMinor: 700, Quantity: 1, VATCode: "1", Subject: "commodity"},
				{Description: "Доставка", AmountMinor: 300, Quantity: 1, VATCode: "1", Subject: "service"},
			},
		}
	}

	t.Run("годный чек", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, payment.CheckReceipt(good(), true, 1000))
	})

	t.Run("чека нет и он не нужен", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, payment.CheckReceipt(nil, false, 1000))
	})

	t.Run("чека нет, а он нужен", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, payment.CheckReceipt(nil, true, 1000), payment.ErrReceiptRequired)
	})

	// Присланный чек проверяется полностью даже при выключенном требовании: он
	// всё равно уедет провайдеру, и негодный отклонит уже созданный платёж.
	t.Run("негодный чек при выключенном требовании", func(t *testing.T) {
		t.Parallel()

		r := good()
		r.Items[0].VATCode = ""

		require.ErrorIs(t, payment.CheckReceipt(r, false, 1000), payment.ErrReceiptInvalid)
	})

	t.Run("телефон вместо почты", func(t *testing.T) {
		t.Parallel()

		r := good()
		r.Customer = payment.Customer{Phone: "+79000000000"}

		assert.NoError(t, payment.CheckReceipt(r, true, 1000))
	})

	// Заполнены оба — проверяются оба: негодный второй контакт отклонит чек так
	// же надёжно, как единственный.
	t.Run("годная почта и негодный телефон", func(t *testing.T) {
		t.Parallel()

		r := good()
		r.Customer.Phone = "+7 900"

		require.ErrorIs(t, payment.CheckReceipt(r, true, 1000), payment.ErrReceiptInvalid)
	})

	t.Run("сумма строк не равна расчёту", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, payment.CheckReceipt(good(), true, 999), payment.ErrReceiptInvalid)
	})

	// Строка на ноль не проходит даже тогда, когда сходится с расчётом: чека на
	// ноль не бывает, а «сумма сошлась» на пустых строках — фискальный документ
	// ни о чём.
	t.Run("строка на ноль", func(t *testing.T) {
		t.Parallel()

		r := good()
		r.Items = []payment.ReceiptItem{{Description: "Ничто", AmountMinor: 0, Quantity: 1, VATCode: "1"}}

		require.ErrorIs(t, payment.CheckReceipt(r, true, 0), payment.ErrReceiptInvalid)
	})

	t.Run("чек без строк", func(t *testing.T) {
		t.Parallel()

		r := good()
		r.Items = nil

		require.ErrorIs(t, payment.CheckReceipt(r, true, 1000), payment.ErrReceiptInvalid)
	})
}

// Маркировка и единица измерения необязательны, проверку проходят и доезжают до
// провайдера как есть: разбирать код «Честного знака» — работа кассы.
func TestReceipt_MarkCodeAndMeasureReachTheProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	req.Receipt.Items[0].MarkCode = "0104603721141719215Qbag!"
	req.Receipt.Items[0].Measure = "piece"

	_, _, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	require.Len(t, h.prov.Created, 1)
	sent := h.prov.Created[0].Receipt
	require.NotNil(t, sent)
	assert.Equal(t, "0104603721141719215Qbag!", sent.Items[0].MarkCode)
	assert.Equal(t, "piece", sent.Items[0].Measure)
}
