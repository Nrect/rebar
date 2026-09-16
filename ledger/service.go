package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/kit/secrets"
)

// Service — книга поверх порта Store: движение, отмена, остаток и проверка
// цепи.
type Service struct {
	store  Store
	book   Book
	keys   map[secrets.KeyID][]byte
	active secrets.KeyID
	now    func() time.Time
	newID  func() uuid.UUID
}

// NewService паникует на nil-хранилище и негодном Config: ошибка конфигурации
// падает на старте, а не на первом движении.
func NewService(store Store, cfg Config) *Service {
	if store == nil {
		panic("ledger.NewService: store must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("ledger.NewService: " + err.Error())
	}
	keys := maps.Clone(cfg.Keys)
	for id, key := range keys {
		keys[id] = slices.Clone(key)
	}
	return &Service{
		store: store, book: cfg.Book.clone(), keys: keys, active: cfg.ActiveKey,
		now:   func() time.Time { return time.Now().UTC() },
		newID: uuid.New,
	}
}

// SetClock подменяет источник времени; только для тестов и до начала работы.
// nil — паника здесь, а не разыменование в чужом стеке.
func (s *Service) SetClock(now func() time.Time) {
	if now == nil {
		panic("ledger.Service.SetClock: now must not be nil")
	}
	s.now = now
}

// WithStore — тот же сервис поверх другого хранилища: так постинг входит в
// транзакцию потребителя (решение 11) — svc.WithStore(store.WithTx(tx)).
// Книга, ключи и часы общие.
func (s *Service) WithStore(store Store) *Service {
	if store == nil {
		panic("ledger.Service.WithStore: store must not be nil")
	}
	c := *s
	c.store = store
	return &c
}

// Post проводит движение. Повтор той же операции с тем же ключом отдаёт
// прежнюю запись; тот же ключ на другую операцию — ErrKeyReused.
//
// Ошибки: ErrInvalidKey, ErrInvalidRequest, ErrUnknownKind — до хранилища;
// ErrKeyReused, ErrInsufficientFunds, ErrUnavailable — под блокировкой.
func (s *Service) Post(ctx context.Context, req PostRequest) (Entry, error) {
	mv, err := s.postMovement(req)
	if err != nil {
		return Entry{}, err
	}
	return s.run(ctx, mv, nil)
}

// Reverse отменяет запись встречной: сумма ровно противоположная, отмена одна
// на запись, отмена отмены запрещена, путь By разрешён родом записи.
//
// Ошибки сверх Post: ErrEntryNotFound, ErrNotReversible, ErrAlreadyReversed.
func (s *Service) Reverse(ctx context.Context, req ReverseRequest) (Entry, error) {
	mv, err := s.reverseMovement(req)
	if err != nil {
		return Entry{}, err
	}
	return s.run(ctx, mv, s.reverseTarget)
}

// Balance — остаток счёта; у счёта без движений — ноль.
func (s *Service) Balance(ctx context.Context, account uuid.UUID) (int64, error) {
	if account == uuid.Nil {
		return 0, fmt.Errorf("%w: account is nil", ErrInvalidRequest)
	}
	head, err := s.store.Account(ctx, s.book.Name, account)
	if err != nil {
		return 0, storeError("read account", err)
	}
	return head.BalanceMinor, nil
}

// decideFunc — шаг решения под блокировкой, после пробы ключа и до подписи.
type decideFunc func(ctx context.Context, tx AccountTx, mv *movement) error

// run — протокол постинга (решение 9): блокировка → проба идемпотентности →
// решение → подпись → вставка.
func (s *Service) run(ctx context.Context, mv movement, decide decideFunc) (Entry, error) {
	var (
		out    Entry
		fnErr  error
		called bool
	)
	err := s.store.Post(ctx, s.book.Name, mv.account, func(tx AccountTx, head Account) error {
		called = true
		out, fnErr = s.locked(ctx, tx, head, mv, decide)
		return fnErr
	})
	switch {
	case fnErr != nil:
		return Entry{}, fnErr
	case err != nil:
		return Entry{}, storeError("post", err)
	case !called:
		return Entry{}, fmt.Errorf("%w: store did not run the posting", ErrUnavailable)
	}
	return out, nil
}

func (s *Service) locked(ctx context.Context, tx AccountTx, head Account, mv movement, decide decideFunc,
) (Entry, error) {
	prior, found, err := tx.EntryByKey(ctx, mv.key)
	switch {
	case err != nil:
		return Entry{}, storeError("probe idempotency key", err)
	case found && !mv.sameAs(prior):
		return Entry{}, fmt.Errorf("%w: entry %s", ErrKeyReused, prior.ID)
	case found:
		return prior, nil
	}
	if decide != nil {
		if decideErr := decide(ctx, tx, &mv); decideErr != nil {
			return Entry{}, decideErr
		}
	}
	e, err := s.seal(mv, head)
	if err != nil {
		return Entry{}, err
	}
	if err := tx.Insert(ctx, e); err != nil {
		return Entry{}, storeError("insert entry", err)
	}
	return e, nil
}

// reverseTarget — гасимая запись под блокировкой: есть на счёте, не отмена,
// род разрешает этот путь, отмены ещё нет. Сумма — ровно противоположная.
func (s *Service) reverseTarget(ctx context.Context, tx AccountTx, mv *movement) error {
	target, found, err := tx.EntryByID(ctx, *mv.reversesID)
	if err != nil {
		return storeError("load entry", err)
	}
	if !found {
		return fmt.Errorf("%w: entry %s", ErrEntryNotFound, *mv.reversesID)
	}
	if target.Kind == KindReversal {
		return fmt.Errorf("%w: entry %s is itself a reversal", ErrNotReversible, target.ID)
	}
	spec, ok := s.book.Spec(target.Kind)
	if !ok || !slices.Contains(spec.ReversibleBy, mv.by) {
		return fmt.Errorf("%w: kind %q of entry %s is not reversible by %q", ErrNotReversible, target.Kind, target.ID, mv.by)
	}
	_, reversed, err := tx.ReversalOf(ctx, target.ID)
	if err != nil {
		return storeError("probe reversal", err)
	}
	if reversed {
		return fmt.Errorf("%w: entry %s", ErrAlreadyReversed, target.ID)
	}
	mv.amount = -target.AmountMinor
	return nil
}

// seal — запись из движения и головы цепи: номер, остаток после с проверкой
// границы, prev_hash и подпись активным ключом.
func (s *Service) seal(mv movement, head Account) (Entry, error) {
	if err := checkAmount(SignAny, mv.amount); err != nil {
		return Entry{}, err
	}
	after, ok := addBalance(head.BalanceMinor, mv.amount)
	if !ok {
		return Entry{}, fmt.Errorf("%w: balance would overflow int64", ErrInvalidRequest)
	}
	if after < s.book.Floor {
		return Entry{}, fmt.Errorf("%w: account %s", ErrInsufficientFunds, mv.account)
	}
	prev, err := head.prevHash()
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		ID: s.newID(), Book: s.book.Name, Account: mv.account, Seq: head.Seq + 1,
		Kind: mv.kind, AmountMinor: mv.amount, BalanceAfterMinor: after,
		Reference: mv.reference, ReversesID: mv.reversesID,
		Reason: mv.reason, Actor: mv.actor, IdempotencyKey: mv.key,
		CreatedAt: moment(s.now()), KeyID: s.active, PrevHash: prev,
	}
	e.EntryHash = sign(s.keys[s.active], e)
	return e, nil
}

// addBalance — сумма без переполнения: при нём слагаемое и результат уходят в
// разные стороны от остатка.
func addBalance(balance, amount int64) (int64, bool) {
	sum := balance + amount
	if amount > 0 && sum < balance || amount < 0 && sum > balance {
		return 0, false
	}
	return sum, true
}

// storeError — сбой хранилища в ErrUnavailable. Ошибки с sentinel книги
// проходят как есть: отказ схемы (контракт Insert) остаётся своим классом, а
// уже завёрнутый сбой не заворачивается дважды.
func storeError(op string, err error) error {
	for _, known := range []error{
		ErrUnavailable, ErrInsufficientFunds, ErrAlreadyReversed, ErrNotReversible,
		ErrEntryNotFound, ErrUnknownKind, ErrInvalidRequest,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}
