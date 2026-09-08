package paymenttest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// Ошибки двойника провайдера: отличимы от доменных, чтобы тест не принял свою
// заглушку за решение пакета.
var (
	// ErrNoEvent — ParseWebhook позвали, а событие в Inbox не положили.
	ErrNoEvent = errors.New("paymenttest: no event in inbox")
	// ErrNoPayment — спросили про платёж, которого у двойника нет.
	ErrNoPayment = errors.New("paymenttest: unknown payment")
	// ErrProviderDown — сбой связи с провайдером: временный, ретрай осмыслен.
	ErrProviderDown = errors.New("paymenttest: provider is down")
)

// MemProvider — двойник payment.Provider.
//
// Идемпотентность смоделирована по-настоящему: повторный вызов с тем же ключом
// возвращает ТОТ ЖЕ результат. Двойник, который на второй вызов выдаёт второй
// платёж, сделал бы зелёным тест на дозавершение брошенной попытки — и скрыл
// бы, что в проде это второе списание.
//
// ProviderEventID детерминированный ("<name>:<paymentID>:<status>"), как того
// требует контракт порта: вебхук и сверка дедуплицируются друг с другом, а не
// применяют одно событие дважды.
type MemProvider struct {
	mu sync.Mutex

	// ProviderName — имя, которое вернёт Name().
	ProviderName payment.ProviderName
	// Created, Captures, Cancels, Refunds — все запросы к провайдеру. По ним
	// тест проверяет, что уехал ПРОИЗВОДНЫЙ ключ, а не клиентский.
	Created  []payment.CreatePaymentRequest
	Captures []payment.CaptureRequest
	Cancels  []string
	Refunds  []payment.RefundProviderRequest
	// Result — шаблон ответа на первое создание платежа. Нулевое значение
	// означает «принял, вот ссылка».
	Result payment.CreatePaymentResult
	// Payments — providerPaymentID → состояние, которое вернёт GetPayment.
	Payments map[string]payment.Event
	// Inbox — события, которые ParseWebhook отдаст по порядку.
	Inbox []payment.Event

	// NoHolds — двухстадийной оплаты нет: Capture и Cancel отвечают
	// ErrUnsupported, как адаптер провайдера без холдов.
	NoHolds bool
	// BadSignature — ParseWebhook вернёт ErrInvalidSignature, НЕ разбирая тело.
	BadSignature bool
	// NoPaymentID — ответить «принял», но платёж не назвать. Так проверяется,
	// что сервис не переводит намерение в pending без id, о котором потом можно
	// спросить сверку.
	NoPaymentID bool
	// RejectFor — ссылки потребителя, по которым провайдер отказывает
	// ДЕТЕРМИНИРОВАННО: платёж не создастся и повтор даст тот же отказ.
	RejectFor map[string]bool
	// FailFor — ссылки, по которым провайдер не отвечает: временный сбой, ретрай
	// осмыслен.
	FailFor map[string]bool
	// CreateHook — зовётся внутри CreatePayment, до ответа. Так тест изображает
	// событие, приехавшее РОВНО между вставкой намерения и ответом провайдера:
	// иначе эту щель не воспроизвести, а именно в ней домен обязан отдать
	// фактическое состояние строки, а не своё ожидание.
	CreateHook func(req payment.CreatePaymentRequest)
	// RefundEcho — если не ноль, Refund отвечает ЭТОЙ суммой вместо запрошенной.
	// Так изображается провайдер, вернувший не то, о чём просили: домен обязан
	// отказаться записывать в книгу цифру, которой не было.
	RefundEcho int64
	// CreateErr, GetErr, RefundErr, CaptureErr, CancelErr, ParseErr — сбои
	// внешнего мира на всех вызовах сразу.
	CreateErr, GetErr, RefundErr, CaptureErr, CancelErr, ParseErr error

	Calls map[string]int

	byKey       map[string]payment.CreatePaymentResult
	byEventKey  map[string]payment.Event
	confirmSeen map[string]bool
}

var _ payment.Provider = (*MemProvider)(nil)

// NewMemProvider создаёт двойник провайдера с именем name.
func NewMemProvider(name payment.ProviderName) *MemProvider {
	return &MemProvider{
		ProviderName: name,
		Payments:     map[string]payment.Event{},
		RejectFor:    map[string]bool{},
		FailFor:      map[string]bool{},
		Calls:        map[string]int{},
		byKey:        map[string]payment.CreatePaymentResult{},
		byEventKey:   map[string]payment.Event{},
		confirmSeen:  map[string]bool{},
	}
}

// Name — имя провайдера.
func (p *MemProvider) Name() payment.ProviderName { return p.ProviderName }

// CreatePayment создаёт платёж; идемпотентен по req.IdempotencyKey.
func (p *MemProvider) CreatePayment(_ context.Context, req payment.CreatePaymentRequest,
) (payment.CreatePaymentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["CreatePayment"]++
	p.Created = append(p.Created, req)
	if p.CreateHook != nil {
		p.CreateHook(req)
	}
	switch {
	case p.CreateErr != nil:
		return payment.CreatePaymentResult{}, p.CreateErr
	case p.FailFor[req.Reference]:
		return payment.CreatePaymentResult{}, fmt.Errorf("%w: reference %q", ErrProviderDown, req.Reference)
	}
	if prev, ok := p.byKey[req.IdempotencyKey]; ok {
		return prev, nil
	}

	res := p.Result
	if p.RejectFor[req.Reference] {
		res = payment.CreatePaymentResult{Status: payment.EventFailed}
	}
	if res.Status == "" {
		res.Status = payment.EventPending
	}
	if res.Status != payment.EventFailed && res.ProviderPaymentID == "" && !p.NoPaymentID {
		res.ProviderPaymentID = "pay-" + req.IntentID.String()
		res.Confirmation = payment.Confirmation{
			Type: payment.ConfirmationRedirect,
			URL:  "https://pay.example/" + res.ProviderPaymentID,
		}
	}
	p.byKey[req.IdempotencyKey] = res
	if res.ProviderPaymentID != "" {
		p.Payments[res.ProviderPaymentID] = p.event(res.ProviderPaymentID, req.IntentID,
			payment.EventPending, req.AmountMinor, req.Currency)
	}
	return res, nil
}

// ParseWebhook отдаёт следующее событие из Inbox.
func (p *MemProvider) ParseWebhook(_ context.Context, _ payment.WebhookRequest,
) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["ParseWebhook"]++
	// Подлинность проверяется ДО разбора тела и до любого похода в БД — двойник
	// повторяет этот порядок, иначе тест «неподтверждённый вебхук не стоит нам
	// ни одного обращения к стору» проверял бы не то.
	if p.BadSignature {
		return payment.Event{}, payment.ErrInvalidSignature
	}
	if p.ParseErr != nil {
		return payment.Event{}, p.ParseErr
	}
	if len(p.Inbox) == 0 {
		return payment.Event{}, ErrNoEvent
	}
	ev := p.Inbox[0]
	p.Inbox = p.Inbox[1:]
	return ev, nil
}

// GetPayment — состояние платежа у провайдера.
func (p *MemProvider) GetPayment(_ context.Context, providerPaymentID string) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["GetPayment"]++
	if p.GetErr != nil {
		return payment.Event{}, p.GetErr
	}
	ev, ok := p.Payments[providerPaymentID]
	if !ok {
		return payment.Event{}, ErrNoPayment
	}
	return ev, nil
}

// Capture списывает холд; идемпотентен по ключу. При NoHolds — ErrUnsupported.
func (p *MemProvider) Capture(_ context.Context, req payment.CaptureRequest) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["Capture"]++
	p.Captures = append(p.Captures, req)
	switch {
	case p.NoHolds:
		return payment.Event{}, fmt.Errorf("%w: two-stage payments", payment.ErrUnsupported)
	case p.CaptureErr != nil:
		return payment.Event{}, p.CaptureErr
	}
	return p.settle(req.IdempotencyKey, req.ProviderPaymentID, payment.EventSucceeded,
		req.AmountMinor, req.Currency), nil
}

// Cancel снимает холд; идемпотентен по ключу. При NoHolds — ErrUnsupported.
func (p *MemProvider) Cancel(_ context.Context, providerPaymentID, idempotencyKey string,
) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["Cancel"]++
	p.Cancels = append(p.Cancels, providerPaymentID)
	switch {
	case p.NoHolds:
		return payment.Event{}, fmt.Errorf("%w: two-stage payments", payment.ErrUnsupported)
	case p.CancelErr != nil:
		return payment.Event{}, p.CancelErr
	}
	return p.settle(idempotencyKey, providerPaymentID, payment.EventCanceled, 0, ""), nil
}

// Refund — возврат на req.AmountMinor; идемпотентен по req.IdempotencyKey.
//
// Идемпотентность смоделирована по-настоящему: повтор с тем же ключом
// возвращает ТОТ ЖЕ возврат, а с другим — второй. Двойник, возвращающий одно и
// то же на любой ключ, сделал бы зелёным тест на повтор после сбоя стора — и
// скрыл бы, что в проде это второе движение денег.
func (p *MemProvider) Refund(_ context.Context, req payment.RefundProviderRequest,
) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls["Refund"]++
	p.Refunds = append(p.Refunds, req)
	if p.RefundErr != nil {
		return payment.Event{}, p.RefundErr
	}
	if prev, ok := p.byEventKey[req.IdempotencyKey]; ok {
		return prev, nil
	}
	echoed := req.AmountMinor
	if p.RefundEcho != 0 {
		echoed = p.RefundEcho
	}
	ev := p.event(req.ProviderPaymentID, uuid.Nil, payment.EventRefunded,
		echoed, req.Currency)
	// У возврата свой id события: он не должен совпасть с id зачисления, иначе
	// дедуп проглотил бы одно как дубль другого.
	ev.ProviderEventID = fmt.Sprintf("%s:refund:%s:%d",
		p.ProviderName, req.ProviderPaymentID, req.AmountMinor)
	p.byEventKey[req.IdempotencyKey] = ev
	return ev, nil
}

// settle — идемпотентный по ключу ответ на списание или отмену; он же обновляет
// состояние платежа, которое потом вернёт GetPayment.
func (p *MemProvider) settle(key, paymentID string, t payment.EventType,
	amountMinor int64, currency string,
) payment.Event {
	if prev, ok := p.byEventKey[key]; ok {
		return prev
	}
	intentID := p.Payments[paymentID].IntentID
	if amountMinor == 0 && currency == "" {
		amountMinor, currency = p.Payments[paymentID].AmountMinor, p.Payments[paymentID].Currency
	}
	ev := p.event(paymentID, intentID, t, amountMinor, currency)
	p.byEventKey[key] = ev
	p.Payments[paymentID] = ev
	return ev
}

// SetPayment кладёт состояние платежа, которое вернёт GetPayment.
func (p *MemProvider) SetPayment(providerPaymentID string, ev payment.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Payments[providerPaymentID] = ev
}

// Push кладёт событие в очередь ParseWebhook.
func (p *MemProvider) Push(ev payment.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Inbox = append(p.Inbox, ev)
}

// CallCount — сколько раз звали метод; через мьютекс, потому что двойник живёт
// и под конкурентными тестами.
func (p *MemProvider) CallCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Calls[method]
}

// RefundKeys — различные ключи идемпотентности, с которыми звали Refund. По их
// числу тест считает, сколько РАЗНЫХ возвратов увидел бы провайдер.
func (p *MemProvider) RefundKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.Refunds))
	seen := map[string]bool{}
	for _, r := range p.Refunds {
		if !seen[r.IdempotencyKey] {
			seen[r.IdempotencyKey] = true
			keys = append(keys, r.IdempotencyKey)
		}
	}
	return keys
}

// Event — событие в форме, которую отдал бы адаптер: детерминированный id и
// привязка к намерению. Удобство для тестов, а не часть порта.
func (p *MemProvider) Event(providerPaymentID string, intentID uuid.UUID, t payment.EventType,
	amountMinor int64, currency string,
) payment.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.event(providerPaymentID, intentID, t, amountMinor, currency)
}

func (p *MemProvider) event(providerPaymentID string, intentID uuid.UUID, t payment.EventType,
	amountMinor int64, currency string,
) payment.Event {
	return payment.Event{
		Provider:          p.ProviderName,
		ProviderEventID:   fmt.Sprintf("%s:%s:%s", p.ProviderName, providerPaymentID, t),
		ProviderPaymentID: providerPaymentID,
		IntentID:          intentID,
		Type:              t,
		AmountMinor:       amountMinor,
		Currency:          currency,
	}
}
