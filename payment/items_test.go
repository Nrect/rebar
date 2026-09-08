package payment_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
)

// CheckItems проверяет ВНУТРЕННЮЮ согласованность состава и ничего больше:
// правило цены — у потребителя, и считается оно до Start.
func TestCheckItems(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		items []payment.OrderItem
		total int64
		max   int
		// wantErr — nil означает годный состав. Потолок ДЕНЕГ отдаёт
		// ErrInvalidMoney, а не ErrInvalidRequest: это разные отказы и разбирают
		// их разные люди.
		wantErr error
	}{
		"годный состав": {items(), testAmount, 10, nil},
		"одна позиция": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: 100, Quantity: 1}}, 100, 10, nil,
		},
		"повтор товара законен: единица состава — позиция": {
			[]payment.OrderItem{
				{Position: 0, ProductID: "a", AmountMinor: 100, Quantity: 1},
				{Position: 1, ProductID: "a", AmountMinor: 60, Quantity: 1},
			}, 160, 10, nil,
		},
		"количество больше единицы": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: 300, Quantity: 3}}, 300, 10, nil,
		},
		"пустой состав": {nil, 0, 10, payment.ErrInvalidRequest},
		"дыра в нумерации": {
			[]payment.OrderItem{
				{Position: 0, ProductID: "a", AmountMinor: 100, Quantity: 1},
				{Position: 2, ProductID: "b", AmountMinor: 60, Quantity: 1},
			}, 160, 10, payment.ErrInvalidRequest,
		},
		"позиции переставлены": {
			[]payment.OrderItem{
				{Position: 1, ProductID: "a", AmountMinor: 100, Quantity: 1},
				{Position: 0, ProductID: "b", AmountMinor: 60, Quantity: 1},
			}, 160, 10, payment.ErrInvalidRequest,
		},
		"сумма не сходится": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: 100, Quantity: 1}}, 101, 10, payment.ErrInvalidRequest,
		},
		"позиция на ноль": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: 0, Quantity: 1}}, 0, 10, payment.ErrInvalidRequest,
		},
		"отрицательная позиция": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: -100, Quantity: 1}}, -100, 10, payment.ErrInvalidRequest,
		},
		"нулевое количество": {
			[]payment.OrderItem{{Position: 0, ProductID: "a", AmountMinor: 100, Quantity: 0}}, 100, 10, payment.ErrInvalidRequest,
		},
		"нет идентификатора товара": {
			[]payment.OrderItem{{Position: 0, ProductID: "", AmountMinor: 100, Quantity: 1}}, 100, 10, payment.ErrInvalidRequest,
		},
		"позиций ровно потолок":  {items(), testAmount, 2, nil},
		"позиций больше потолка": {items(), testAmount, 1, payment.ErrInvalidRequest},
		"потолок не задан":       {items(), testAmount, 0, payment.ErrInvalidRequest},
		"позиция сверх потолка денег": {
			[]payment.OrderItem{{
				Position: 0, ProductID: "a", AmountMinor: payment.MaxMoneyMinor + 1, Quantity: 1,
			}}, payment.MaxMoneyMinor + 1, 10, payment.ErrInvalidMoney,
		},
		"итог сверх потолка денег при законных позициях": {
			[]payment.OrderItem{
				{Position: 0, ProductID: "a", AmountMinor: payment.MaxMoneyMinor, Quantity: 1},
				{Position: 1, ProductID: "b", AmountMinor: payment.MaxMoneyMinor, Quantity: 1},
			}, 2 * payment.MaxMoneyMinor, 10, payment.ErrInvalidMoney,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := payment.CheckItems(tc.items, tc.total, tc.max)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// Потолок, забытый в Config, — это сломанная сборка, а не слишком большой
// заказ: отказ обязан называть именно её, иначе дежурный пойдёт искать
// стотысячную корзину, которой не было.
func TestCheckItems_BrokenCapIsNamedSeparately(t *testing.T) {
	t.Parallel()

	err := payment.CheckItems(items(), testAmount, 0)

	require.ErrorIs(t, err, payment.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "item cap must be positive")
}

// Состав — снапшот: правка среза вызывающим после Start не должна доезжать до
// намерения.
func TestStart_ItemsAreSnapshotted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	in := h.start(t, req)

	req.Items[0].AmountMinor = 1
	req.Items[0].ProductID = "подменили"

	stored := h.mustIntent(t, in.ID)
	assert.Equal(t, "book-1", stored.Items[0].ProductID)
	assert.Equal(t, int64(79900), stored.Items[0].AmountMinor)
}
