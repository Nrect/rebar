package ledgertest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nrect/rebar/ledger"
)

// suiteDebitRace — условие выпуска 5: N параллельных списаний при остатке на M
// дают ровно M успехов, остаток ноль, номера без дыр, и КАЖДАЯ подпись
// пересчитана. Проиграть здесь можно только нехваткой остатка: любая другая
// ошибка означает, что блокировка счёта не сериализует постинг.
func suiteDebitRace(t *testing.T, f fixture) {
	t.Helper()
	const unit, funded, attempts = 7, 12, 48
	account := uuid.New()
	f.post(t, topup(account, unit*funded, "race-fund"))

	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			_, err := f.svc.Post(t.Context(), spend(account, unit, fmt.Sprintf("race-spend-%d", i)))
			results <- err
		})
	}
	wg.Wait()
	close(results)

	succeeded, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ledger.ErrInsufficientFunds):
			refused++
		default:
			t.Errorf("параллельное списание: неожиданная ошибка: %v", err)
		}
	}
	equal(t, succeeded, funded, "успешных списаний")
	equal(t, refused, attempts-funded, "отказов по остатку")

	entries := f.chain(t, account, funded+1)
	head := f.head(t, account)
	equal(t, head.BalanceMinor, int64(0), "остаток после гонки")
	equal(t, head.Seq, int64(funded+1), "номер головы после гонки")

	// Контроль: проверка цепи видит правку ровно там, где она есть, — иначе
	// «каждая подпись пересчитана» выше ничего не доказывает.
	tampered := slices.Clone(entries)
	tampered[funded/2].Reason = "tampered"
	v := f.svc.VerifyEntries(account, ledger.Position{}, tampered)
	want := []ledger.Mismatch{{EntryID: tampered[funded/2].ID, Seq: tampered[funded/2].Seq, Check: ledger.CheckSignature}}
	isTrue(t, slices.Equal(v.Mismatches, want), fmt.Sprintf("правка причины: расхождения %v, ожидалось %v", v.Mismatches, want))
}

// suiteKeyRace — условие выпуска 6: N одновременных попыток одного ключа дают
// одну запись, и каждая попытка получает именно её. Счёт новый: гонка идёт и
// за его заведение.
func suiteKeyRace(t *testing.T, f fixture) {
	t.Helper()
	const attempts = 32
	account := uuid.New()
	req := topup(account, 900, "race-same-key")

	ids := make(chan uuid.UUID, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			e, err := f.svc.Post(t.Context(), req)
			if err != nil {
				t.Errorf("попытка с тем же ключом: %v", err)
				return
			}
			ids <- e.ID
		})
	}
	wg.Wait()
	close(ids)

	entries := f.chain(t, account, 1)
	for id := range ids {
		equal(t, id, entries[0].ID, "каждая попытка получает единственную запись")
	}
}
