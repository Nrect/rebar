package ledgertest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/ledger"
)

// Ошибки двойника, отличимые от доменных. Завёрнуты в sentinel модуля с тем
// классом, который на этом месте даст адаптер (ADR-0007, «Двойники»).
var (
	// ErrTxDone — счёт тронут после того, как Post вернулся: у адаптера
	// транзакция к этому моменту закрыта.
	ErrTxDone = fmt.Errorf("%w: ledgertest: locked account used after Post returned", ledger.ErrUnavailable)
	// ErrUnknownBook — книга не заведена в двойнике: у адаптера это строка
	// справочника книг, которой нет, то есть дефект сборки.
	ErrUnknownBook = fmt.Errorf("%w: ledgertest: book is not registered in the store", ledger.ErrInvalidRequest)
)

// MemStore — ledger.Store в памяти. Держит то, что у ledgerpg держат схема и
// триггеры: номер ровно +1, цепь prev_hash, остаток после, нижнюю границу
// книги, род и знак по справочнику, ключ в пределах счёта, одну отмену на
// запись ровно противоположной суммой. Подпись не проверяет — ключа у
// хранилища нет.
//
// ЗАМОК ДВОЙНИКА И ЕСТЬ ТРАНЗАКЦИЯ: Post держит его всё время fn, как адаптер
// держит строку счёта, поэтому fn хранилище мимо tx не трогает — повиснет.
// Вставки fn применяются после её успеха; ошибка fn не оставляет ни одной.
//
// ПУБЛИЧНЫХ ПОЛЕЙ НЕТ: ручки — методы под тем же замком, под которым их читают
// методы порта (CONVENTIONS §3).
type MemStore struct {
	mu       sync.Mutex
	books    map[string]ledger.Book
	accounts map[accountKey]*account
	err      error
	calls    map[string]int
}

var _ ledger.Store = (*MemStore)(nil)

type accountKey struct {
	book    string
	account uuid.UUID
}

// account — голова хранится отдельно от записей, как строка счёта у адаптера.
type account struct {
	head    ledger.Account
	entries []ledger.Entry
}

// NewMemStore — пустое хранилище с заведёнными книгами: у адаптера это строки
// справочников книг и родов. Паникует на негодной книге и на повторе имени.
func NewMemStore(books ...ledger.Book) *MemStore {
	if len(books) == 0 {
		panic("ledgertest.NewMemStore: at least one book must be registered")
	}
	m := &MemStore{
		books:    make(map[string]ledger.Book, len(books)),
		accounts: map[accountKey]*account{},
		calls:    map[string]int{},
	}
	for _, book := range books {
		if err := book.Validate(); err != nil {
			panic("ledgertest.NewMemStore: " + err.Error())
		}
		if _, dup := m.books[book.Name]; dup {
			panic(fmt.Sprintf("ledgertest.NewMemStore: book %q is registered twice", book.Name))
		}
		m.books[book.Name] = cloneBook(book)
	}
	return m
}

// SetErr — если не nil, каждый метод порта отвечает ею в ledger.ErrUnavailable,
// как сбой адаптера; nil снимает. Изнутри fn не звать — замок уже взят.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// CallCount — сколько раз звали метод порта (Post, Account, Entries, Accounts,
// EntryByKey, EntryByID, ReversalOf, Insert): для утверждений «до хранилища не
// дошли».
func (m *MemStore) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

// Post — блокировка счёта и fn под ней (контракт ledger.Store.Post).
func (m *MemStore) Post(ctx context.Context, book string, account uuid.UUID,
	fn func(tx ledger.AccountTx, head ledger.Account) error,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Post"]++
	if err := m.fail(ctx, "post"); err != nil {
		return err
	}
	spec, ok := m.books[book]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownBook, book)
	}
	key := accountKey{book: book, account: account}
	tx := &accountTx{store: m, book: spec, key: key, head: m.head(key)}
	defer tx.done.Store(true)
	if err := fn(tx, cloneHead(tx.head)); err != nil {
		return err
	}
	// Фиксация не проходит ни после отказа вставки, ни по отменённому контексту.
	if tx.aborted {
		return errAborted("commit")
	}
	if err := ctx.Err(); err != nil {
		return storeError("commit", err)
	}
	m.commit(key, tx.pending)
	return nil
}

// Account — счёт без блокировки.
func (m *MemStore) Account(ctx context.Context, book string, account uuid.UUID) (ledger.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Account"]++
	if err := m.fail(ctx, "account"); err != nil {
		return ledger.Account{}, err
	}
	return m.head(accountKey{book: book, account: account}), nil
}

// Entries — записи после afterSeq по возрастанию номера, не больше limit.
func (m *MemStore) Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int,
) ([]ledger.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Entries"]++
	// Потолок адаптер проверяет до запроса: отмена его не перебивает.
	if limit <= 0 {
		return nil, fmt.Errorf("%w: ledgertest: limit must be positive, got %d", ledger.ErrInvalidRequest, limit)
	}
	if err := m.fail(ctx, "entries"); err != nil {
		return nil, err
	}
	out := make([]ledger.Entry, 0, min(limit, 16))
	if acct := m.accounts[accountKey{book: book, account: account}]; acct != nil {
		for _, e := range acct.entries {
			if e.Seq > afterSeq && len(out) < limit {
				out = append(out, copyEntry(e))
			}
		}
	}
	return out, nil
}

// Accounts — счета книги после after по возрастанию байтов uuid, не больше
// limit. Записей без строки счёта у двойника не бывает: запись живёт в счёте.
func (m *MemStore) Accounts(ctx context.Context, book string, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Accounts"]++
	if limit <= 0 {
		return nil, fmt.Errorf("%w: ledgertest: limit must be positive, got %d", ledger.ErrInvalidRequest, limit)
	}
	if err := m.fail(ctx, "accounts"); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, min(limit, len(m.accounts)))
	for key := range m.accounts {
		if key.book == book && bytes.Compare(key.account[:], after[:]) > 0 {
			ids = append(ids, key.account)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids[:min(limit, len(ids))], nil
}

// fail — отменённый контекст и заданный сбой: оба в ledger.ErrUnavailable, как
// у адаптера, у которого драйвер откажет по отменённому ctx. Стоит там, где у
// адаптера первый поход в базу.
func (m *MemStore) fail(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return storeError(op, err)
	}
	if m.err != nil {
		return storeError(op, m.err)
	}
	return nil
}

func (m *MemStore) head(key accountKey) ledger.Account {
	if acct := m.accounts[key]; acct != nil {
		return cloneHead(acct.head)
	}
	return ledger.Account{}
}

// commit — счёт заводится и без вставок: адаптер заводит строку счёта при
// блокировке, и она фиксируется вместе с транзакцией Post.
func (m *MemStore) commit(key accountKey, pending []ledger.Entry) {
	acct := m.accounts[key]
	if acct == nil {
		acct = &account{}
		m.accounts[key] = acct
	}
	if len(pending) == 0 {
		return
	}
	acct.entries = append(acct.entries, pending...)
	acct.head = headAfter(pending[len(pending)-1])
}

// idTaken — id записи занят в любом счёте: первичный ключ у адаптера общий.
func (m *MemStore) idTaken(id uuid.UUID) bool {
	for _, acct := range m.accounts {
		if slices.ContainsFunc(acct.entries, func(e ledger.Entry) bool { return e.ID == id }) {
			return true
		}
	}
	return false
}

// accountTx — счёт под замком Post.
type accountTx struct {
	store *MemStore
	book  ledger.Book
	key   accountKey
	// head — голова с учётом вставок fn; pending — вставки до фиксации.
	head    ledger.Account
	pending []ledger.Entry
	// aborted — вставка уже отказала: у базы транзакция после ошибки не
	// принимает ничего, и двойник не мягче неё.
	aborted bool
	done    atomic.Bool
}

// EntryByKey — проба идемпотентности в пределах счёта.
func (a *accountTx) EntryByKey(ctx context.Context, key string) (ledger.Entry, bool, error) {
	if err := a.enter(ctx, "EntryByKey"); err != nil {
		return ledger.Entry{}, false, err
	}
	e, ok := a.find(func(e ledger.Entry) bool { return e.IdempotencyKey == key })
	return e, ok, nil
}

// EntryByID — запись этого счёта.
func (a *accountTx) EntryByID(ctx context.Context, id uuid.UUID) (ledger.Entry, bool, error) {
	if err := a.enter(ctx, "EntryByID"); err != nil {
		return ledger.Entry{}, false, err
	}
	e, ok := a.find(func(e ledger.Entry) bool { return e.ID == id })
	return e, ok, nil
}

// ReversalOf — отмена записи id.
func (a *accountTx) ReversalOf(ctx context.Context, id uuid.UUID) (ledger.Entry, bool, error) {
	if err := a.enter(ctx, "ReversalOf"); err != nil {
		return ledger.Entry{}, false, err
	}
	e, ok := a.find(func(e ledger.Entry) bool { return e.ReversesID != nil && *e.ReversesID == id })
	return e, ok, nil
}

// Insert — вставка с проверками схемы (контракт ledger.AccountTx.Insert).
// Чужой счёт адаптер видит до запроса: база не знает, какой счёт заблокирован.
func (a *accountTx) Insert(ctx context.Context, e ledger.Entry) error {
	if a.done.Load() {
		return ErrTxDone
	}
	a.store.calls["Insert"]++
	if e.Book != a.key.book || e.Account != a.key.account {
		return fmt.Errorf("%w: ledgertest: entry %s belongs to another account", ledger.ErrInvalidRequest, e.ID)
	}
	if err := a.live(ctx, "Insert"); err != nil {
		return err
	}
	e = copyEntry(e)
	e.CreatedAt = dbMoment(e.CreatedAt)
	for _, check := range []func(ledger.Entry) error{a.checkColumns, a.checkReversal, a.checkChain} {
		if err := check(e); err != nil {
			a.aborted = true
			return err
		}
	}
	a.pending = append(a.pending, e)
	a.head = headAfter(e)
	return nil
}

// enter — вход в метод счёта: закрытая транзакция, затем live. Замок уже у
// Post; после него счёт не трогает ни одного поля хранилища.
func (a *accountTx) enter(ctx context.Context, method string) error {
	if a.done.Load() {
		return ErrTxDone
	}
	a.store.calls[method]++
	return a.live(ctx, method)
}

// live — транзакция ещё принимает запросы: вставка не отказывала, контекст
// не отменён, сбой не задан.
func (a *accountTx) live(ctx context.Context, method string) error {
	if a.aborted {
		return errAborted(method)
	}
	return a.store.fail(ctx, method)
}

// find — запись счёта среди зафиксированных и вставок fn; копией.
func (a *accountTx) find(match func(ledger.Entry) bool) (ledger.Entry, bool) {
	var committed []ledger.Entry
	if acct := a.store.accounts[a.key]; acct != nil {
		committed = acct.entries
	}
	for _, list := range [][]ledger.Entry{committed, a.pending} {
		if i := slices.IndexFunc(list, match); i >= 0 {
			return copyEntry(list[i]), true
		}
	}
	return ledger.Entry{}, false
}

// checkColumns — CHECK и внешние ключи строки: род по справочнику, знак и
// сумма, обязательные поля рода, ключ, длины подписей, ReversesID ровно у
// отмены.
func (a *accountTx) checkColumns(e ledger.Entry) error {
	spec, ok := a.book.Spec(e.Kind)
	if !ok {
		return fmt.Errorf("%w: ledgertest: %q", ledger.ErrUnknownKind, e.Kind)
	}
	if !amountFits(spec.Sign, e.AmountMinor) || !requiredPresent(spec, e) || !columnsWellFormed(e) {
		return fmt.Errorf("%w: ledgertest: entry %s breaks a column check of kind %q",
			ledger.ErrInvalidRequest, e.ID, e.Kind)
	}
	return nil
}

func amountFits(sign ledger.Sign, amount int64) bool {
	switch {
	case amount == 0, amount > ledger.MaxAmountMinor, amount < -ledger.MaxAmountMinor:
		return false
	case sign == ledger.SignCredit:
		return amount > 0
	case sign == ledger.SignDebit:
		return amount < 0
	default:
		return true
	}
}

func requiredPresent(spec ledger.KindSpec, e ledger.Entry) bool {
	if spec.Reference == ledger.Required && e.Reference == "" {
		return false
	}
	return spec.Attribution != ledger.Required || e.Reason != "" && e.Actor != ""
}

func columnsWellFormed(e ledger.Entry) bool {
	return e.IdempotencyKey != "" &&
		len(e.PrevHash) == ledger.HashSize && len(e.EntryHash) == ledger.HashSize &&
		(e.Kind == ledger.KindReversal) == (e.ReversesID != nil)
}

// checkReversal — отмена гасит запись этого счёта, которая сама не отмена,
// ровно противоположной суммой и один раз: у адаптера триггер и частичный
// уникальный индекс.
func (a *accountTx) checkReversal(e ledger.Entry) error {
	if e.ReversesID == nil {
		return nil
	}
	target, ok := a.find(func(x ledger.Entry) bool { return x.ID == *e.ReversesID })
	switch {
	case !ok:
		return fmt.Errorf("%w: ledgertest: entry %s", ledger.ErrEntryNotFound, *e.ReversesID)
	case target.Kind == ledger.KindReversal:
		return fmt.Errorf("%w: ledgertest: entry %s is a reversal", ledger.ErrNotReversible, target.ID)
	case e.AmountMinor != -target.AmountMinor:
		return fmt.Errorf("%w: ledgertest: reversal of entry %s is not the opposite amount", ledger.ErrInvalidRequest, target.ID)
	}
	if _, reversed := a.find(func(x ledger.Entry) bool { return x.ReversesID != nil && *x.ReversesID == target.ID }); reversed {
		return fmt.Errorf("%w: ledgertest: entry %s", ledger.ErrAlreadyReversed, target.ID)
	}
	return nil
}

// checkChain — голова счёта: номер +1, prev_hash, остаток после, граница
// книги, свободные ключ и id. У адаптера нарушение головы — 40001, и
// транзакцию повторяют.
func (a *accountTx) checkChain(e ledger.Entry) error {
	prev := a.head.LastHash
	if a.head.Seq == 0 {
		prev = make([]byte, ledger.HashSize)
	}
	after, overflow := addBalance(a.head.BalanceMinor, e.AmountMinor)
	switch {
	case overflow:
		return fmt.Errorf("%w: ledgertest: balance of seq %d overflows", ledger.ErrInvalidRequest, e.Seq)
	case e.Seq != a.head.Seq+1:
		return raceLost(fmt.Sprintf("seq %d does not follow head seq %d", e.Seq, a.head.Seq))
	case !bytes.Equal(e.PrevHash, prev):
		return raceLost(fmt.Sprintf("prev_hash of seq %d is not the head hash", e.Seq))
	case e.BalanceAfterMinor != after:
		return raceLost(fmt.Sprintf("balance_after of seq %d does not follow the head balance", e.Seq))
	case e.BalanceAfterMinor < a.book.Floor:
		return fmt.Errorf("%w: ledgertest: seq %d", ledger.ErrInsufficientFunds, e.Seq)
	}
	if _, taken := a.find(func(x ledger.Entry) bool { return x.IdempotencyKey == e.IdempotencyKey }); taken {
		return raceLost(fmt.Sprintf("idempotency key of seq %d is taken", e.Seq))
	}
	if a.store.idTaken(e.ID) || slices.ContainsFunc(a.pending, func(x ledger.Entry) bool { return x.ID == e.ID }) {
		return raceLost(fmt.Sprintf("entry id %s is taken", e.ID))
	}
	return nil
}

func addBalance(balance, amount int64) (sum int64, overflow bool) {
	sum = balance + amount
	return sum, amount > 0 && sum < balance || amount < 0 && sum > balance
}

// errAborted — вызов в транзакции, где вставка уже отказала: у базы это 25P02.
func errAborted(op string) error {
	return fmt.Errorf("%w: ledgertest: %s: transaction is aborted by a failed insert", ledger.ErrUnavailable, op)
}

func raceLost(what string) error {
	return fmt.Errorf("%w: ledgertest: %s", ledger.ErrUnavailable, what)
}

func storeError(op string, err error) error {
	return fmt.Errorf("%w: ledgertest: %s: %w", ledger.ErrUnavailable, op, err)
}

func headAfter(e ledger.Entry) ledger.Account {
	return ledger.Account{Seq: e.Seq, BalanceMinor: e.BalanceAfterMinor, LastHash: bytes.Clone(e.EntryHash)}
}

func cloneHead(h ledger.Account) ledger.Account {
	h.LastHash = bytes.Clone(h.LastHash)
	return h
}

// copyEntry — копия до последнего указателя: правка полученной записи не
// доезжает до «базы».
func copyEntry(e ledger.Entry) ledger.Entry {
	if e.ReversesID != nil {
		id := *e.ReversesID
		e.ReversesID = &id
	}
	e.PrevHash = bytes.Clone(e.PrevHash)
	e.EntryHash = bytes.Clone(e.EntryHash)
	return e
}

func cloneBook(b ledger.Book) ledger.Book {
	kinds := make([]ledger.KindSpec, len(b.Kinds))
	for i, kind := range b.Kinds {
		kind.ReversibleBy = slices.Clone(kind.ReversibleBy)
		kinds[i] = kind
	}
	b.Kinds = kinds
	return b
}

// dbMoment — момент так, как его хранит timestamptz.
func dbMoment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
