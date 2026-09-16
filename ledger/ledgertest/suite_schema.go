package ledgertest

import (
	"testing"

	"github.com/google/uuid"

	"github.com/nrect/rebar/ledger"
)

// schemaRefusal — запись мимо ядра, нарушающая ровно один инвариант схемы.
type schemaRefusal struct {
	name string
	// spoil портит годное списание; base — пополнение на 1000, первая запись счёта.
	spoil func(e *ledger.Entry, base ledger.Entry)
	want  error
}

// schemaRefusals — вторая линия: всё, что ядро проверило под блокировкой,
// хранилище проверяет ещё раз (контракт ledger.AccountTx.Insert).
var schemaRefusals = []schemaRefusal{
	{"номер не head+1", func(e *ledger.Entry, _ ledger.Entry) { e.Seq++ }, ledger.ErrUnavailable},
	{"prev_hash не подпись последней записи", func(e *ledger.Entry, _ ledger.Entry) { e.PrevHash = randomHash() }, ledger.ErrUnavailable},
	{"остаток после не сходится", func(e *ledger.Entry, _ ledger.Entry) { e.BalanceAfterMinor-- }, ledger.ErrUnavailable},
	{"ключ занят", func(e *ledger.Entry, base ledger.Entry) { e.IdempotencyKey = base.IdempotencyKey }, ledger.ErrUnavailable},
	{"id занят", func(e *ledger.Entry, base ledger.Entry) { e.ID = base.ID }, ledger.ErrUnavailable},
	{"ниже границы", func(e *ledger.Entry, _ ledger.Entry) { e.AmountMinor, e.BalanceAfterMinor = -1001, -1 }, ledger.ErrInsufficientFunds},
	{"рода нет в справочнике", func(e *ledger.Entry, _ ledger.Entry) { e.Kind = "unknown_kind" }, ledger.ErrUnknownKind},
	{"знак не по роду", func(e *ledger.Entry, _ ledger.Entry) { e.AmountMinor, e.BalanceAfterMinor = 100, 1100 }, ledger.ErrInvalidRequest},
	{"нулевая сумма", func(e *ledger.Entry, _ ledger.Entry) { e.AmountMinor, e.BalanceAfterMinor = 0, 1000 }, ledger.ErrInvalidRequest},
	{"сумма за потолком", overCap, ledger.ErrInvalidRequest},
	{"нет обязательного основания", func(e *ledger.Entry, _ ledger.Entry) { e.Reference = "" }, ledger.ErrInvalidRequest},
	{"нет автора у рода с автором", withoutActor, ledger.ErrInvalidRequest},
	{"подпись не той длины", func(e *ledger.Entry, _ ledger.Entry) { e.EntryHash = e.EntryHash[:16] }, ledger.ErrInvalidRequest},
	{"пустой ключ", func(e *ledger.Entry, _ ledger.Entry) { e.IdempotencyKey = "" }, ledger.ErrInvalidRequest},
	{"запись не этого счёта", func(e *ledger.Entry, _ ledger.Entry) { e.Account = uuid.New() }, ledger.ErrInvalidRequest},
	{"ReversesID не у отмены", func(e *ledger.Entry, base ledger.Entry) { e.ReversesID = &base.ID }, ledger.ErrInvalidRequest},
	{"отмена без ReversesID", func(e *ledger.Entry, _ ledger.Entry) { e.Kind = ledger.KindReversal }, ledger.ErrInvalidRequest},
	{"отмена записи не этого счёта", reversalOfStranger, ledger.ErrEntryNotFound},
	{"отмена не ровно противоположной суммой", partialReversal, ledger.ErrInvalidRequest},
}

func overCap(e *ledger.Entry, _ ledger.Entry) {
	e.Kind = kindAdjust
	e.AmountMinor = ledger.MaxAmountMinor + 1
	e.BalanceAfterMinor = 1000 + e.AmountMinor
}

func withoutActor(e *ledger.Entry, _ ledger.Entry) {
	e.Kind = kindAdjust
	e.Actor = ""
}

func reversalOfStranger(e *ledger.Entry, _ ledger.Entry) {
	stranger := uuid.New()
	e.Kind, e.ReversesID = ledger.KindReversal, &stranger
}

func partialReversal(e *ledger.Entry, base ledger.Entry) {
	e.Kind, e.ReversesID = ledger.KindReversal, &base.ID
	e.AmountMinor, e.BalanceAfterMinor = -base.AmountMinor+1, 1
}

func suiteSchemaRefusals(t *testing.T, f fixture) {
	t.Helper()
	for _, tc := range schemaRefusals {
		account := uuid.New()
		base := f.post(t, topup(account, 1000, "schema-base"))
		err := f.insert(t, account, func(head ledger.Account) ledger.Entry {
			e := f.rawEntry(account, head, kindSpend, -100, "schema-raw")
			tc.spoil(&e, base)
			return e
		})
		errIs(t, err, tc.want, tc.name)
		f.chain(t, account, 1)
	}
}

// suiteSchemaReversals — одна отмена на запись, запрет отмены отмены и гасимую
// запись на своём счёте держит схема, а не только ядро (уточнение арбитра 5).
// Остатка хватает, чтобы граница не сработала раньше.
func suiteSchemaReversals(t *testing.T, f fixture) {
	t.Helper()
	account, stranger := uuid.New(), uuid.New()
	base := f.post(t, topup(account, 1000, "schema-rev-base"))
	f.post(t, topup(account, 5000, "schema-rev-cushion"))
	rev := f.reverse(t, reversal(account, base.ID, byOperator, "schema-rev"))
	theirs := f.post(t, topup(stranger, 1000, "schema-rev-theirs"))

	err := f.insert(t, account, func(head ledger.Account) ledger.Entry {
		e := f.rawEntry(account, head, ledger.KindReversal, -base.AmountMinor, "schema-rev-again")
		e.ReversesID = &base.ID
		return e
	})
	errIs(t, err, ledger.ErrAlreadyReversed, "вторая отмена мимо ядра")

	err = f.insert(t, account, func(head ledger.Account) ledger.Entry {
		e := f.rawEntry(account, head, ledger.KindReversal, -rev.AmountMinor, "schema-rev-of-rev")
		e.ReversesID = &rev.ID
		return e
	})
	errIs(t, err, ledger.ErrNotReversible, "отмена отмены мимо ядра")

	// Запись есть в книге, но на другом счёте: сумма, цепь и граница сходятся,
	// отбить обязан именно поиск гасимой записи на своём счёте.
	err = f.insert(t, account, func(head ledger.Account) ledger.Entry {
		e := f.rawEntry(account, head, ledger.KindReversal, -theirs.AmountMinor, "schema-rev-foreign")
		e.ReversesID = &theirs.ID
		return e
	})
	errIs(t, err, ledger.ErrEntryNotFound, "отмена записи другого счёта мимо ядра")
	f.chain(t, account, 3)
	f.chain(t, stranger, 1)
	equal(t, f.head(t, stranger).BalanceMinor, int64(1000), "остаток чужого счёта")
}
