package ledgertest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/nrect/rebar/ledger"
)

// orderedAccounts — четыре счёта в известном порядке байтов uuid: порядок
// задаёт первый байт.
func orderedAccounts() []uuid.UUID {
	ids := make([]uuid.UUID, 0, 4)
	for _, first := range []byte{0x10, 0x20, 0x30, 0x40} {
		id := uuid.New()
		id[0] = first
		ids = append(ids, id)
	}
	return ids
}

// suiteAccounts — обход сверки: счета книги по возрастанию байтов uuid после
// курсора, не больше потолка. Счёт, по которому Post прошёл без вставки, в
// выборке есть: адаптер заводит его строку при блокировке.
func suiteAccounts(t *testing.T, f fixture) {
	t.Helper()
	ids := orderedAccounts()
	// Порядок заведения не совпадает с порядком uuid.
	for _, i := range []int{2, 0, 3} {
		f.post(t, topup(ids[i], 100, fmt.Sprintf("accounts-%d", i)))
	}
	noErr(t, f.store.Post(t.Context(), f.book.Name, ids[1], func(ledger.AccountTx, ledger.Account) error {
		return nil
	}), "Post без вставки")

	for _, tc := range []struct {
		after uuid.UUID
		limit int
		want  []uuid.UUID
	}{
		{uuid.Nil, 3, ids[:3]},
		{ids[2], 3, ids[3:]},
		{ids[3], 3, nil},
		{ids[0], 1, ids[1:2]},
		{uuid.Nil, 100, ids},
	} {
		got, err := f.store.Accounts(t.Context(), f.book.Name, tc.after, tc.limit)
		noErr(t, err, "обход счетов")
		isTrue(t, slices.Equal(got, tc.want),
			fmt.Sprintf("после %s по %d: счета %v, ожидались %v", tc.after, tc.limit, got, tc.want))
	}
	other, err := f.store.Accounts(t.Context(), "suite_other_book", uuid.Nil, 10)
	noErr(t, err, "обход другой книги")
	equal(t, len(other), 0, "счетов другой книги")
	for _, limit := range []int{0, -1} {
		_, err = f.store.Accounts(t.Context(), f.book.Name, uuid.Nil, limit)
		errIs(t, err, ledger.ErrInvalidRequest, fmt.Sprintf("потолок %d", limit))
	}
}

// suiteReconcile — сверка по хранилищу: за круг обход доходит до каждого счёта
// ровно раз, проверяет каждую запись, включая отмену, и расхождений на годной
// книге не находит; после круга — снова с начала. Порции меньше счетов и
// записей: курсор и потолок работают на обеих реализациях одинаково.
func suiteReconcile(t *testing.T, f fixture) {
	t.Helper()
	ids := orderedAccounts()
	for i, account := range ids {
		f.post(t, topup(account, 1000, fmt.Sprintf("reconcile-%d-in", i)))
		out := f.post(t, spend(account, 100, fmt.Sprintf("reconcile-%d-out", i)))
		f.reverse(t, reversal(account, out.ID, byOrders, fmt.Sprintf("reconcile-%d-back", i)))
	}
	obs := NewObserver()
	rec := ledger.NewReconciler(f.svc, obs, ledger.ReconcileConfig{Accounts: 3, Page: 2})

	checked := make([]int, 0, 3)
	for range 3 {
		n, err := rec.Run(t.Context())
		noErr(t, err, "прогон сверки")
		checked = append(checked, n)
	}
	isTrue(t, slices.Equal(checked, []int{9, 3, 9}),
		fmt.Sprintf("проверено записей по прогонам %v, ожидалось [9 3 9]: три счёта, последний, снова с начала", checked))
	equal(t, len(obs.Findings()), 0, "расхождений на годной книге")
	isTrue(t, slices.Equal(obs.Watched(), []string{f.book.Name}), "книга заведена у наблюдателя")
}
