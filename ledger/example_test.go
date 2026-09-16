package ledger_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Кошелёк: пополнение, списание сверх остатка и отмена пополнения.
//
// В проде хранилище — адаптер Postgres, и движение идёт в транзакции
// бизнес-факта: svc.WithStore(store.WithTx(tx)).Post(ctx, req). Отличие теста
// от прода — один конструктор хранилища.
func Example() {
	ctx := context.Background()
	// Секрет приложения — из окружения; ключ книги выводится с purpose = имя книги.
	appSecret := bytes.Repeat([]byte{0x42}, secrets.MinSecretLen)
	key, err := secrets.DeriveKey(appSecret, "wallet")
	if err != nil {
		panic(err)
	}

	book := ledger.Book{
		Name: "wallet", Unit: "RUB",
		Kinds: []ledger.KindSpec{
			{Name: "topup", Sign: ledger.SignCredit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{"support"}},
			{Name: "order_payment", Sign: ledger.SignDebit, Reference: ledger.Required, Attribution: ledger.Optional},
		},
	}
	store := ledgertest.NewMemStore(book)
	svc := ledger.NewService(store, ledger.Config{Book: book, Keys: map[secrets.KeyID][]byte{1: key}, ActiveKey: 1})

	customer := uuid.MustParse("7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f")
	in, err := svc.Post(ctx, ledger.PostRequest{
		Account: customer, Kind: "topup", AmountMinor: 1000, Reference: "payment:81", IdempotencyKey: "payment:81",
	})
	if err != nil {
		panic(err)
	}
	_, err = svc.Post(ctx, ledger.PostRequest{
		Account: customer, Kind: "order_payment", AmountMinor: -1500, Reference: "order:17", IdempotencyKey: "order:17",
	})
	fmt.Println("списание сверх остатка:", errors.Is(err, ledger.ErrInsufficientFunds))

	rev, err := svc.Reverse(ctx, ledger.ReverseRequest{
		Account: customer, EntryID: in.ID, By: "support",
		Reason: "payment charged back", Actor: "staff:12", IdempotencyKey: "chargeback:81",
	})
	if err != nil {
		panic(err)
	}
	balance, err := svc.Balance(ctx, customer)
	if err != nil {
		panic(err)
	}
	check, err := svc.Verify(ctx, customer, ledger.Position{}, 100)
	if err != nil {
		panic(err)
	}
	fmt.Println(rev.Kind, rev.AmountMinor, "остаток", balance)
	fmt.Println("проверено записей:", check.Checked, "расхождений:", len(check.Mismatches))
	// Output:
	// списание сверх остатка: true
	// reversal -1000 остаток 0
	// проверено записей: 2 расхождений: 0
}
