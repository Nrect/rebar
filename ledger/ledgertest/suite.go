package ledgertest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/ledger"
)

// StoreFactory — пустое хранилище с заведённой книгой, своё на каждый
// сценарий. Каждый Post у него — своя транзакция из пула: иначе гонки набора
// не гонки.
type StoreFactory func(t *testing.T, book ledger.Book) ledger.Store

// RunStoreSuite — контрактный набор порта ledger.Store.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ: двойник и ledgerpg не вправе разойтись, иначе
// тесты потребителя зелены на двойнике при сломанном проде. Сценарии идут
// через ledger.Service там, где нужна подпись, и мимо него там, где
// проверяется схема. В наборе условия выпуска 5 и 6 ADR-0009: гонка списаний
// и гонка одного ключа.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("ledgertest.RunStoreSuite: newStore must not be nil")
	}
	for _, sc := range storeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			book := suiteBook()
			if sc.book != nil {
				book = sc.book()
			}
			sc.run(t, newFixture(t, newStore(t, book), book))
		})
	}
}

type storeScenario struct {
	name string
	// book — книга сценария; nil — suiteBook.
	book func() ledger.Book
	run  func(t *testing.T, f fixture)
}

var storeScenarios = []storeScenario{
	{name: "движения ложатся цепью: номер +1, остаток в записи, подпись сходится", run: suitePostChain},
	{name: "повтор той же операции отдаёт прежнюю запись байт в байт", run: suiteReplay},
	{name: "тот же ключ на другую операцию — ErrKeyReused; ключ живёт в счёте", run: suiteKeyReused},
	{name: "ниже границы — ErrInsufficientFunds и ничего не записано", run: suiteFloor},
	{name: "граница — значение книги: минус до Floor и не дальше", book: overdraftBook, run: suiteOverdraft},
	{name: "отмена: встречная сумма, одна на запись, отмена отмены запрещена", run: suiteReverse},
	{name: "отмену разрешает род и остаток", run: suiteReverseRefusals},
	{name: "N параллельных списаний при остатке на M — ровно M успехов", run: suiteDebitRace},
	{name: "N одновременных попыток одного ключа — одна запись", run: suiteKeyRace},
	{name: "моменты — как из timestamptz: UTC и микросекунды", run: suiteMoments},
	{name: "схема отвергает вставку мимо ядра", run: suiteSchemaRefusals},
	{name: "схема держит одну отмену, запрет отмены отмены и гасимую запись на своём счёте", run: suiteSchemaReversals},
	{name: "ошибка fn не оставляет вставок, счёт вне fn не живёт", run: suiteRollback},
	{name: "отказ вставки обрывает транзакцию fn", run: suiteAbortedTx},
	{name: "чтение по курсору и потолку; непозитивный потолок — ошибка", run: suiteReads},
	{name: "обход счетов книги: порядок uuid, курсор и потолок; непозитивный потолок — ошибка", run: suiteAccounts},
	{name: "сверка проходит книгу целиком за круг и не находит расхождений", run: suiteReconcile},
	{name: "отменённый контекст — ErrUnavailable и ничего не записано", run: suiteCancelled},
	{name: "записи и голова отдаются копией", run: suiteCopies},
}

func suitePostChain(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	posted := []ledger.Entry{
		f.post(t, topup(account, 1000, "chain-1")),
		f.post(t, spend(account, 300, "chain-2")),
		f.post(t, adjust(account, -200, "chain-3")),
	}

	entries := f.chain(t, account, len(posted))
	for i := range posted {
		sameEntry(t, entries[i], posted[i], fmt.Sprintf("запись %d прочитана", i+1))
	}
	isTrue(t, bytes.Equal(entries[0].PrevHash, make([]byte, ledger.HashSize)), "prev_hash первой записи — 32 нулевых байта")
	equal(t, entries[2].BalanceAfterMinor, int64(500), "остаток после третьей записи")

	head := f.head(t, account)
	equal(t, head.Seq, int64(3), "номер головы")
	equal(t, head.BalanceMinor, int64(500), "остаток головы")
	isTrue(t, bytes.Equal(head.LastHash, entries[2].EntryHash), "голова держит подпись последней записи")

	balance, err := f.svc.Balance(t.Context(), account)
	noErr(t, err, "остаток")
	equal(t, balance, int64(500), "остаток счёта")
}

func suiteReplay(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	req := adjust(account, 700, "replay")
	first := f.post(t, req)

	req.IdempotencyKey = "  replay  "
	again := f.post(t, req)
	sameEntry(t, again, first, "повтор отдаёт прежнюю запись")
	f.chain(t, account, 1)
}

func suiteKeyReused(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	f.post(t, topup(account, 700, "reused"))

	otherReason := topup(account, 700, "reused")
	otherReason.Reason = "another reason"
	for _, tc := range []struct {
		what string
		req  ledger.PostRequest
	}{
		{"другая сумма", topup(account, 701, "reused")},
		{"другой род", adjust(account, 700, "reused")},
		{"другая причина", otherReason},
	} {
		_, err := f.svc.Post(t.Context(), tc.req)
		errIs(t, err, ledger.ErrKeyReused, tc.what)
	}
	_, err := f.svc.Reverse(t.Context(), reversal(account, uuid.New(), byOperator, "reused"))
	errIs(t, err, ledger.ErrKeyReused, "отмена под ключом движения")
	f.chain(t, account, 1)

	other := f.post(t, topup(uuid.New(), 701, "reused"))
	equal(t, other.Seq, int64(1), "тот же ключ на другом счёте — новое движение")
}

func suiteFloor(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	f.post(t, topup(account, 100, "floor-in"))

	_, err := f.svc.Post(t.Context(), spend(account, 101, "floor-over"))
	errIs(t, err, ledger.ErrInsufficientFunds, "списание сверх остатка")
	f.chain(t, account, 1)
	equal(t, f.head(t, account).BalanceMinor, int64(100), "остаток после отказа")

	f.post(t, spend(account, 100, "floor-exact"))
	equal(t, f.head(t, account).BalanceMinor, int64(0), "списание ровно до границы")
}

func suiteOverdraft(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	f.post(t, spend(account, 500, "overdraft-in"))
	equal(t, f.head(t, account).BalanceMinor, int64(-500), "минус до границы книги")

	_, err := f.svc.Post(t.Context(), spend(account, 1, "overdraft-over"))
	errIs(t, err, ledger.ErrInsufficientFunds, "за границу книги")
	f.chain(t, account, 1)
}

func suiteReverse(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	in := f.post(t, topup(account, 500, "rev-in"))

	rev := f.reverse(t, reversal(account, in.ID, byOperator, "rev-1"))
	equal(t, rev.Kind, ledger.KindReversal, "род отмены")
	equal(t, rev.AmountMinor, int64(-500), "сумма ровно противоположная")
	equal(t, rev.BalanceAfterMinor, int64(0), "остаток после отмены")
	isTrue(t, rev.ReversesID != nil && *rev.ReversesID == in.ID, "отмена ссылается на гасимую запись")

	again := f.reverse(t, reversal(account, in.ID, byOperator, "rev-1"))
	sameEntry(t, again, rev, "повтор отмены с тем же ключом")

	_, err := f.svc.Reverse(t.Context(), reversal(account, in.ID, byOperator, "rev-2"))
	errIs(t, err, ledger.ErrAlreadyReversed, "вторая отмена")
	_, err = f.svc.Reverse(t.Context(), reversal(account, rev.ID, byOperator, "rev-3"))
	errIs(t, err, ledger.ErrNotReversible, "отмена отмены")
	f.chain(t, account, 2)
}

func suiteReverseRefusals(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	in := f.post(t, topup(account, 300, "refuse-in"))
	out := f.post(t, spend(account, 200, "refuse-out"))

	_, err := f.svc.Reverse(t.Context(), reversal(account, out.ID, byOperator, "refuse-1"))
	errIs(t, err, ledger.ErrNotReversible, "род отменяет только свой путь")
	_, err = f.svc.Reverse(t.Context(), reversal(account, uuid.New(), byOperator, "refuse-2"))
	errIs(t, err, ledger.ErrEntryNotFound, "записи нет")
	_, err = f.svc.Reverse(t.Context(), reversal(uuid.New(), in.ID, byOperator, "refuse-3"))
	errIs(t, err, ledger.ErrEntryNotFound, "запись чужого счёта")
	_, err = f.svc.Reverse(t.Context(), reversal(account, in.ID, byOperator, "refuse-4"))
	errIs(t, err, ledger.ErrInsufficientFunds, "отмена пополнения, которое уже потрачено")
	f.chain(t, account, 2)

	back := f.reverse(t, reversal(account, out.ID, byOrders, "refuse-5"))
	equal(t, back.BalanceAfterMinor, int64(300), "свой путь отменяет списание")
	f.chain(t, account, 3)
}

func suiteMoments(t *testing.T, f fixture) {
	t.Helper()
	at := time.Date(2026, 9, 16, 15, 4, 5, 123456789, time.FixedZone("UTC+3", 3*60*60))
	f.clock.Set(at)
	account := uuid.New()
	posted := f.post(t, topup(account, 100, "moment"))
	isTrue(t, sameMoment(posted.CreatedAt, at), "Post отдаёт момент в UTC и до микросекунд: "+posted.CreatedAt.String())
	stored := f.chain(t, account, 1)[0]
	isTrue(t, sameMoment(stored.CreatedAt, at), "хранилище отдаёт момент в UTC и до микросекунд: "+stored.CreatedAt.String())

	raw := uuid.New()
	noErr(t, f.insert(t, raw, func(head ledger.Account) ledger.Entry {
		e := f.rawEntry(raw, head, kindTopup, 100, "raw-moment")
		e.CreatedAt = at
		return e
	}), "вставка момента мимо ядра")
	got := f.entries(t, raw)[0].CreatedAt
	isTrue(t, sameMoment(got, at), "хранилище само усекает момент вставки: "+got.String())
}

func suiteRollback(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	errRefused := errors.New("ledgertest: fn refused")
	var leaked ledger.AccountTx
	err := f.store.Post(t.Context(), f.book.Name, account, func(tx ledger.AccountTx, head ledger.Account) error {
		leaked = tx
		if err := tx.Insert(t.Context(), f.rawEntry(account, head, kindTopup, 100, "rolled")); err != nil {
			return err
		}
		return errRefused
	})
	errIs(t, err, errRefused, "ошибка fn отдаётся как есть")
	equal(t, len(f.entries(t, account)), 0, "вставка fn после её ошибки")
	equal(t, f.head(t, account).Seq, int64(0), "голова после ошибки fn")

	_, _, err = leaked.EntryByKey(t.Context(), "rolled")
	errIs(t, err, ledger.ErrUnavailable, "счёт вне fn")

	e := f.post(t, topup(account, 100, "rolled"))
	equal(t, e.Seq, int64(1), "номер и ключ после отката свободны")
}

// suiteAbortedTx — после отказа вставки транзакция не принимает ничего, даже
// если fn отказ проглотила: у базы это «current transaction is aborted».
func suiteAbortedTx(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	var refused, next error
	err := f.store.Post(t.Context(), f.book.Name, account, func(tx ledger.AccountTx, head ledger.Account) error {
		refused = tx.Insert(t.Context(), f.rawEntry(account, head, kindSpend, -1, "aborted-over"))
		next = tx.Insert(t.Context(), f.rawEntry(account, head, kindTopup, 100, "aborted-next"))
		return nil
	})
	errIs(t, refused, ledger.ErrInsufficientFunds, "вставка ниже границы")
	errIs(t, next, ledger.ErrUnavailable, "вставка после отказа")
	errIs(t, err, ledger.ErrUnavailable, "фиксация после отказа")
	equal(t, len(f.entries(t, account)), 0, "записи после оборванной транзакции")
}

func suiteReads(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	for i := range 5 {
		f.post(t, topup(account, int64(i+1), fmt.Sprintf("read-%d", i)))
	}
	for _, tc := range []struct {
		after int64
		limit int
		want  []int64
	}{
		{0, 2, []int64{1, 2}},
		{2, 2, []int64{3, 4}},
		{4, 10, []int64{5}},
		{5, 10, []int64{}},
	} {
		page, err := f.store.Entries(t.Context(), f.book.Name, account, tc.after, tc.limit)
		noErr(t, err, "страница записей")
		seqs := make([]int64, 0, len(page))
		for _, e := range page {
			seqs = append(seqs, e.Seq)
		}
		isTrue(t, slices.Equal(seqs, tc.want),
			fmt.Sprintf("после %d по %d: номера %v, ожидались %v", tc.after, tc.limit, seqs, tc.want))
	}
	for _, limit := range []int{0, -1} {
		_, err := f.store.Entries(t.Context(), f.book.Name, account, 0, limit)
		errIs(t, err, ledger.ErrInvalidRequest, fmt.Sprintf("потолок %d", limit))
	}

	unknown := uuid.New()
	head := f.head(t, unknown)
	isTrue(t, head.Seq == 0 && head.BalanceMinor == 0 && len(head.LastHash) == 0, "голова счёта без движений нулевая")
	equal(t, len(f.entries(t, unknown)), 0, "записи счёта без движений")
}

func suiteCancelled(t *testing.T, f fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	account := uuid.New()
	called := false
	err := f.store.Post(ctx, f.book.Name, account, func(ledger.AccountTx, ledger.Account) error {
		called = true
		return nil
	})
	cancelledIs(t, err, "Post")
	isTrue(t, !called, "fn по отменённому контексту не зовётся")
	_, err = f.store.Account(ctx, f.book.Name, account)
	cancelledIs(t, err, "Account")
	_, err = f.store.Entries(ctx, f.book.Name, account, 0, 10)
	cancelledIs(t, err, "Entries")
	_, err = f.store.Accounts(ctx, f.book.Name, uuid.Nil, 10)
	cancelledIs(t, err, "Accounts")

	late, cancelLate := context.WithCancel(t.Context())
	defer cancelLate()
	err = f.store.Post(late, f.book.Name, account, func(tx ledger.AccountTx, head ledger.Account) error {
		if insertErr := tx.Insert(late, f.rawEntry(account, head, kindTopup, 100, "late")); insertErr != nil {
			return insertErr
		}
		cancelLate()
		return nil
	})
	cancelledIs(t, err, "фиксация по отменённому контексту")
	equal(t, len(f.entries(t, account)), 0, "записи после отменённой фиксации")
}

func cancelledIs(t *testing.T, err error, what string) {
	t.Helper()
	errIs(t, err, ledger.ErrUnavailable, what)
	errIs(t, err, context.Canceled, what)
}

func suiteCopies(t *testing.T, f fixture) {
	t.Helper()
	account := uuid.New()
	in := f.post(t, topup(account, 100, "copy-in"))
	f.reverse(t, reversal(account, in.ID, byOperator, "copy-rev"))

	got := f.entries(t, account)
	got[0].EntryHash[0] ^= 0xff
	got[1].PrevHash[0] ^= 0xff
	*got[1].ReversesID = uuid.New()
	head := f.head(t, account)
	head.LastHash[0] ^= 0xff

	f.chain(t, account, 2)
	isTrue(t, bytes.Equal(f.head(t, account).LastHash, f.entries(t, account)[1].EntryHash), "голова не тронута правкой копии")
}
