package shoppg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// ErrUnknownOrder — намерение ссылается на заказ, которого нет.
//
// ЭТО ОТКАЗ, А НЕ ПРЕДУПРЕЖДЕНИЕ: хук уронит транзакцию, зачисление
// откатится, провайдер получит 503 и повторит. Пометить оплату «как-нибудь»
// нельзя — деньги уже наши, а товара, за который их взяли, нет.
var ErrUnknownOrder = errors.New("shoppg: заказ намерения не найден")

// GrantsOf — какие права открывает оплаченное намерение. Правило каталога
// принадлежит потребителю: тулкит про предметы не знает (payment/doc.go,
// «Чего в пакете НЕТ», п. 5).
//
// Состав берётся из in.Items — снапшота, замороженного в намерении.
type GrantsOf func(in payment.Intent) []entitlement.Grant

// EventOf — событие, которое ложится в outbox тем же коммитом. Пустой Kind
// означает «события нет».
type EventOf func(in payment.Intent, entry payment.LedgerEntry) (outbox.Envelope, error)

// Settler — хук paymentpg: эффекты потребителя в транзакции зачисления и
// возврата (payment/ports.go, «Хук потребителя»).
//
// # Порядок блокировок
//
// Продолжает порядок адаптера платежей — намерение → события → книга → хук —
// СВОИМИ строками: заказ → выдачи (по отсортированному item_id) → outbox.
// Два порядка обхода тех же строк это дедлок ровно в момент оплаты, то есть
// 503 на оплате (paymentpg/doc.go, «Порядок блокировок»).
type Settler struct {
	orders *Orders
	grants *Entitlements
	queue  *outboxpg.Store
	of     GrantsOf
	paid   EventOf
	back   EventOf
}

var _ paymentpg.Settler = (*Settler)(nil)

// NewSettler паникует на nil-зависимости: ошибка проводки падает на старте, а
// не на первой оплате.
func NewSettler(db *DB, queue *outboxpg.Store, of GrantsOf, paid, back EventOf) *Settler {
	if db == nil || queue == nil || of == nil || paid == nil || back == nil {
		panic("shoppg.NewSettler: все зависимости обязательны")
	}
	return &Settler{
		orders: NewOrders(db), grants: NewEntitlements(db), queue: queue,
		of: of, paid: paid, back: back,
	}
}

// OnSettled — заказ оплачен, право выдано, событие в outbox. ВСЁ ЭТО В ТОЙ ЖЕ
// ТРАНЗАКЦИИ, что и книга платежей: падение между шагами оставило бы человека
// заплатившим и не получившим.
//
// Ошибка отсюда откатывает всё, включая строку дедупа события, и уезжает
// наружу как 503 — провайдер повторит (paymentpg/doc.go, п. 2).
func (s *Settler) OnSettled(ctx context.Context, tx pgx.Tx, in payment.Intent,
	entry payment.LedgerEntry,
) error {
	order, err := s.lockOrder(ctx, tx, in)
	if err != nil {
		return err
	}
	if err := s.orders.WithTx(tx).MarkPaid(ctx, order.ID, entry.CreatedAt); err != nil {
		return err
	}
	if err := s.grantAll(ctx, tx, order.SubjectID, s.of(in), entry); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, s.paid, in, entry)
}

// OnRefunded — деньги вернули: право отзывается, событие уезжает в outbox.
//
// paid_at заказа НЕ СБРАСЫВАЕТСЯ: возврат не отменяет факта оплаты, а
// переписывать историю в строке, по которой считают выручку, нечем — книга
// платежей append-only, и заказ обязан читаться так же.
func (s *Settler) OnRefunded(ctx context.Context, tx pgx.Tx, in payment.Intent,
	entry payment.LedgerEntry,
) error {
	order, err := s.lockOrder(ctx, tx, in)
	if err != nil {
		return err
	}
	grants := s.grants.WithTx(tx)
	for _, g := range sortedGrants(s.of(in)) {
		if err := grants.Revoke(ctx, order.SubjectID, g.ItemID); err != nil {
			return err
		}
	}
	return s.enqueue(ctx, tx, s.back, in, entry)
}

// lockOrder — первая собственная строка хука, под FOR UPDATE.
func (s *Settler) lockOrder(ctx context.Context, tx pgx.Tx, in payment.Intent) (Order, error) {
	id, err := uuid.Parse(in.Reference)
	if err != nil {
		// Ни ссылки, ни суммы в тексте: ошибка называет намерение, а не то,
		// что пришло снаружи (payment/doc.go, «Безопасность», п. 12).
		return Order{}, fmt.Errorf("%w: намерение %s", ErrUnknownOrder, in.ID)
	}
	order, found, err := s.orders.WithTx(tx).Lock(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if !found {
		return Order{}, fmt.Errorf("%w: намерение %s", ErrUnknownOrder, in.ID)
	}
	return order, nil
}

// grantAll выдаёт права В ОТСОРТИРОВАННОМ ПОРЯДКЕ. Две одновременные оплаты
// пересекающихся заказов, взявшие строки в противоположном порядке, — это
// дедлок в момент оплаты.
func (s *Settler) grantAll(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID,
	grants []entitlement.Grant, entry payment.LedgerEntry,
) error {
	store := s.grants.WithTx(tx)
	for _, g := range sortedGrants(grants) {
		if err := store.GrantAt(ctx, subjectID, g, entry.CreatedAt, SourcePurchase); err != nil {
			return err
		}
	}
	return nil
}

// sortedGrants — копия набора в детерминированном порядке. Копия, а не
// сортировка на месте: срез принадлежит вызывающему.
func sortedGrants(in []entitlement.Grant) []entitlement.Grant {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b entitlement.Grant) int {
		return strings.Compare(a.ItemID, b.ItemID)
	})
	return out
}

// enqueue кладёт событие в outbox тем же коммитом. Пустой Kind означает
// «события нет» — так потребитель отключает уведомление, не трогая хук.
func (s *Settler) enqueue(ctx context.Context, tx pgx.Tx, of EventOf,
	in payment.Intent, entry payment.LedgerEntry,
) error {
	env, err := of(in, entry)
	if err != nil {
		return err
	}
	if env.Kind == "" {
		return nil
	}
	res, err := s.queue.WithTx(tx).Enqueue(ctx, env)
	if err != nil {
		return err
	}
	// ГРОМКАЯ ИДЕМПОТЕНТНОСТЬ: тот же ключ на другое событие — отказ, а не
	// тихий no-op (outbox/doc.go, п. 5).
	_, err = outbox.CheckDuplicate(env, res)
	return err
}
