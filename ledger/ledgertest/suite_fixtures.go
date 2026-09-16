package ledgertest

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
)

// Данные набора. Роды покрывают все знаки и обе обязательности; пути отмены —
// «свой контур» и «чужой».
const (
	suiteBookName = "suite_wallet"
	kindTopup     = "topup"
	kindSpend     = "spend"
	kindAdjust    = "adjustment"
	byOperator    = "operator"
	byOrders      = "orders"
)

// suiteNow — часы сервиса набора: время порту приходит параметром, и набору
// настоящие часы не нужны.
var suiteNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

func suiteBook() ledger.Book {
	return ledger.Book{
		Name: suiteBookName, Unit: "RUB",
		Kinds: []ledger.KindSpec{
			{
				Name: kindTopup, Sign: ledger.SignCredit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{byOperator},
			},
			{
				Name: kindSpend, Sign: ledger.SignDebit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{byOrders},
			},
			{Name: kindAdjust, Sign: ledger.SignAny, Reference: ledger.Optional, Attribution: ledger.Required},
		},
	}
}

// overdraftBook — та же книга, которой разрешён минус до -500.
func overdraftBook() ledger.Book {
	book := suiteBook()
	book.Name, book.Floor = "suite_overdraft", -500
	return book
}

// fixture — хранилище сценария и сервис поверх него со своим ключом и часами.
type fixture struct {
	store ledger.Store
	book  ledger.Book
	svc   *ledger.Service
	clock *Clock
}

func newFixture(t *testing.T, store ledger.Store, book ledger.Book) fixture {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("ключ подписи набора: %v", err)
	}
	f := fixture{store: store, book: book, clock: NewClock(suiteNow)}
	f.svc = ledger.NewService(store, ledger.Config{
		Book: book, Keys: map[secrets.KeyID][]byte{1: key}, ActiveKey: 1,
	})
	f.svc.SetClock(f.clock.Now)
	return f
}

func topup(account uuid.UUID, amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: account, Kind: kindTopup, AmountMinor: amount, Reference: "payment:" + key, IdempotencyKey: key,
	}
}

func spend(account uuid.UUID, amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: account, Kind: kindSpend, AmountMinor: -amount, Reference: "order:" + key, IdempotencyKey: key,
	}
}

func adjust(account uuid.UUID, amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: account, Kind: kindAdjust, AmountMinor: amount,
		Reason: "manual correction", Actor: "staff:1", IdempotencyKey: key,
	}
}

func reversal(account, entryID uuid.UUID, by, key string) ledger.ReverseRequest {
	return ledger.ReverseRequest{
		Account: account, EntryID: entryID, By: by,
		Reason: "mistaken movement", Actor: "staff:1", IdempotencyKey: key,
	}
}

func (f fixture) post(t *testing.T, req ledger.PostRequest) ledger.Entry {
	t.Helper()
	e, err := f.svc.Post(t.Context(), req)
	noErr(t, err, "движение "+req.IdempotencyKey)
	return e
}

func (f fixture) reverse(t *testing.T, req ledger.ReverseRequest) ledger.Entry {
	t.Helper()
	e, err := f.svc.Reverse(t.Context(), req)
	noErr(t, err, "отмена "+req.IdempotencyKey)
	return e
}

func (f fixture) head(t *testing.T, account uuid.UUID) ledger.Account {
	t.Helper()
	head, err := f.store.Account(t.Context(), f.book.Name, account)
	noErr(t, err, "голова счёта")
	return head
}

func (f fixture) entries(t *testing.T, account uuid.UUID) []ledger.Entry {
	t.Helper()
	entries, err := f.store.Entries(t.Context(), f.book.Name, account, 0, 1000)
	noErr(t, err, "записи счёта")
	return entries
}

// chain — записи счёта, их ровно n, и цепь сходится целиком: номера без дыр,
// prev_hash, остаток после и КАЖДАЯ подпись пересчитана.
func (f fixture) chain(t *testing.T, account uuid.UUID, n int) []ledger.Entry {
	t.Helper()
	entries := f.entries(t, account)
	if len(entries) != n {
		t.Fatalf("записей на счёте %d, ожидалось %d", len(entries), n)
	}
	v := f.svc.VerifyEntries(account, ledger.Position{}, entries)
	if v.Checked != n || len(v.Mismatches) != 0 {
		t.Fatalf("цепь не сходится: проверено %d из %d, расхождения %v", v.Checked, n, v.Mismatches)
	}
	return entries
}

// insert — вставка мимо ядра, одной транзакцией Post.
func (f fixture) insert(t *testing.T, account uuid.UUID, build func(head ledger.Account) ledger.Entry) error {
	t.Helper()
	return f.store.Post(t.Context(), f.book.Name, account, func(tx ledger.AccountTx, head ledger.Account) error {
		return tx.Insert(t.Context(), build(head))
	})
}

// rawEntry — запись мимо ядра, годная для схемы: номер, цепь и остаток по
// голове. Подпись — случайные байты: схема её не проверяет.
func (f fixture) rawEntry(account uuid.UUID, head ledger.Account, kind string, amount int64, key string) ledger.Entry {
	prev := head.LastHash
	if head.Seq == 0 {
		prev = make([]byte, ledger.HashSize)
	}
	return ledger.Entry{
		ID: uuid.New(), Book: f.book.Name, Account: account, Seq: head.Seq + 1,
		Kind: kind, AmountMinor: amount, BalanceAfterMinor: head.BalanceMinor + amount,
		Reference: "raw:" + key, Reason: "raw insert", Actor: "suite", IdempotencyKey: key,
		CreatedAt: suiteNow, KeyID: 1, PrevHash: bytes.Clone(prev), EntryHash: randomHash(),
	}
}

func randomHash() []byte {
	hash := make([]byte, ledger.HashSize)
	_, _ = rand.Read(hash)
	return hash
}

// sameMoment — got равен want так, как want вернётся из timestamptz: в UTC и
// до микросекунд. Голый Equal зоны не видит.
func sameMoment(got, want time.Time) bool {
	return got.Location() == time.UTC && got.Equal(want.Truncate(time.Microsecond))
}

// sameEntry — запись прочитана байт в байт: всё, что легло в подпись, и сама
// подпись.
func sameEntry(t *testing.T, got, want ledger.Entry, what string) {
	t.Helper()
	if !samePlace(got, want) || !sameMovement(got, want) || !sameSeal(got, want) {
		t.Fatalf("%s: получено %+v, ожидалось %+v", what, got, want)
	}
}

func samePlace(a, b ledger.Entry) bool {
	return a.ID == b.ID && a.Book == b.Book && a.Account == b.Account && a.Seq == b.Seq
}

func sameMovement(a, b ledger.Entry) bool {
	if a.ReversesID != nil && b.ReversesID != nil {
		if *a.ReversesID != *b.ReversesID {
			return false
		}
	} else if a.ReversesID != b.ReversesID {
		return false
	}
	return a.Kind == b.Kind && a.AmountMinor == b.AmountMinor && a.BalanceAfterMinor == b.BalanceAfterMinor &&
		a.Reference == b.Reference && a.Reason == b.Reason && a.Actor == b.Actor
}

func sameSeal(a, b ledger.Entry) bool {
	return a.IdempotencyKey == b.IdempotencyKey && sameMoment(a.CreatedAt, b.CreatedAt) && a.KeyID == b.KeyID &&
		bytes.Equal(a.PrevHash, b.PrevHash) && bytes.Equal(a.EntryHash, b.EntryHash)
}

// Проверки набора на голом testing: ledgertest собирается у потребителя без
// тестовых зависимостей (CONVENTIONS §3). Каждая называет, что не сошлось.

func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: неожиданная ошибка: %v", what, err)
	}
}

func errIs(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: ожидалась ошибка %v, получено %v", what, want, err)
	}
}

func equal[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: получено %v, ожидалось %v", what, got, want)
	}
}

func isTrue(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatal(what)
	}
}
