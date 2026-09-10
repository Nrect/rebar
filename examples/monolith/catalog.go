package monolith

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/payment"
)

// Product — товар витрины: цена и то, что он открывает.
//
// ЦЕНУ СЧИТАЕТ ПОТРЕБИТЕЛЬ И ДО Start. Клиент называет только товар и ключ
// идемпотентности; «проверим сумму на фронте» — это отсутствие проверки
// (payment/start.go).
type Product struct {
	Code        string
	Title       string
	AmountMinor int64
	// Items — предметы, которые открывает покупка. Идентификаторы каталога
	// потребителя, непрозрачные для entitlement.
	Items []string
	// TTL — срок доступа; ноль означает бессрочно.
	TTL time.Duration
}

// catalog — витрина примера. Двух товаров хватает, чтобы у хука было что
// сортировать: порядок взятия строк выдач обязан быть детерминированным.
var catalog = map[string]Product{
	"course-basic": {
		Code: "course-basic", Title: "Базовый курс", AmountMinor: 149000,
		Items: []string{"lesson-01", "lesson-02"}, TTL: 365 * 24 * time.Hour,
	},
	"course-pro": {
		Code: "course-pro", Title: "Продвинутый курс", AmountMinor: 349000,
		Items: []string{"lesson-01", "lesson-02", "lesson-03"},
	},
}

// productOf — товар по коду; ok == false — такого товара нет.
func productOf(code string) (Product, bool) {
	p, ok := catalog[code]
	return p, ok
}

// itemsOf — состав расчёта для payment.Start. Одна позиция: пример продаёт
// товар целиком, а не корзину.
func (p Product) itemsOf() []payment.OrderItem {
	return []payment.OrderItem{{
		Position: 0, ProductID: p.Code, Title: p.Title,
		AmountMinor: p.AmountMinor, Quantity: 1,
	}}
}

// grantsOf — какие права открывает оплаченное намерение.
//
// Состав берётся ИЗ НАМЕРЕНИЯ (in.Items), а не из витрины: витрина могла
// поменяться между «показали кнопку» и «пришло событие», а сделка заморожена
// (payment/doc.go, «Безопасность», п. 7).
func grantsOf(in payment.Intent) []entitlement.Grant {
	var out []entitlement.Grant
	for _, item := range in.Items {
		product, ok := productOf(item.ProductID)
		if !ok {
			continue
		}
		out = append(out, product.grants(in.CreatedAt)...)
	}
	return out
}

func (p Product) grants(at time.Time) []entitlement.Grant {
	out := make([]entitlement.Grant, 0, len(p.Items))
	for _, item := range p.Items {
		g := entitlement.Grant{ItemID: item}
		if p.TTL > 0 {
			// Именно указатель, а не нулевое время: нулевое лежит в прошлом, и
			// «забыл заполнить» превратилось бы в вечный отказ.
			expires := at.Add(p.TTL)
			g.ExpiresAt = &expires
		}
		out = append(out, g)
	}
	return out
}

// Типы событий outbox. Набор закрыт и объявлен в outbox.Config.Kinds.
const (
	kindOrderPaid     outbox.Kind = "order.paid"
	kindOrderRefunded outbox.Kind = "order.refunded"
)

// outboxKinds — то, что уезжает в outbox.Config.Kinds.
func outboxKinds() []outbox.Kind { return []outbox.Kind{kindOrderPaid, kindOrderRefunded} }

// orderEvent — тело события. Ни суммы карты, ни описания с персональными
// данными: событие переживёт и заказ, и подписку на него.
type orderEvent struct {
	OrderID     uuid.UUID `json:"order_id"`
	SubjectID   uuid.UUID `json:"subject_id"`
	IntentID    uuid.UUID `json:"intent_id"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
}

// eventOf — конверт outbox по намерению и записи книги.
//
// КЛЮЧ ДЕДУПА ВЫВОДИТСЯ ИЗ ФАКТА — из id строки книги: она уникальна, она
// append-only, и повтор вебхука даст тот же ключ. Ключ, выведенный из момента
// времени, удвоил бы событие на повторной доставке.
func (a *App) eventOf(kind outbox.Kind) func(payment.Intent, payment.LedgerEntry) (outbox.Envelope, error) {
	return func(in payment.Intent, entry payment.LedgerEntry) (outbox.Envelope, error) {
		orderID, err := uuid.Parse(in.Reference)
		if err != nil {
			return outbox.Envelope{}, err
		}
		payload, err := json.Marshal(orderEvent{
			OrderID: orderID, SubjectID: in.PayerID, IntentID: in.ID,
			AmountMinor: entry.AmountMinor, Currency: entry.Currency,
		})
		if err != nil {
			return outbox.Envelope{}, err
		}
		return a.producer.Prepare(outbox.Message{
			Kind: kind, Payload: payload, SchemaVersion: 1,
			DedupKey:      string(kind) + ":" + entry.ID.String(),
			AggregateType: "order", AggregateID: orderID.String(),
			OccurredAt: entry.CreatedAt,
		})
	}
}
