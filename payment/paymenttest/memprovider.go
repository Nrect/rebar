package paymenttest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// Ошибки двойника провайдера: отличимы от доменных, чтобы тест не принял свою
// заглушку за решение пакета.
var (
	// ErrNoEvent — ParseWebhook позвали, а событие в очередь (Push) не положили.
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
//
// ПУБЛИЧНЫХ ПОЛЕЙ НЕТ. Всё, что методы читают под замком, правится только
// методами под тем же замком: тест потребителя меняет ручки, пока ручка его
// HTTP-сервера в другой горутине зовёт провайдер, и поле, записанное мимо
// замка, дало бы гонку под -race — у потребителя, а не у нас.
type MemProvider struct {
	mu sync.Mutex

	name payment.ProviderName

	// Запросы к провайдеру по порядку: по ним тест проверяет, что уехал
	// ПРОИЗВОДНЫЙ ключ, а не клиентский.
	created  []payment.CreatePaymentRequest
	captures []payment.CaptureRequest
	cancels  []string
	refunds  []payment.RefundProviderRequest

	// result — шаблон ответа на первое создание платежа; нулевое значение —
	// «принял, вот ссылка».
	result payment.CreatePaymentResult
	// payments — providerPaymentID → состояние, которое вернёт GetPayment.
	payments map[string]payment.Event
	// inbox — события, которые ParseWebhook отдаст по порядку.
	inbox []payment.Event

	noHolds      bool
	badSignature bool
	noPaymentID  bool
	rejectFor    map[string]bool
	rejectNext   int
	failFor      map[string]bool
	createHook   func(req payment.CreatePaymentRequest)
	refundEcho   int64

	createErr, getErr, refundErr, captureErr, cancelErr, parseErr error

	calls map[string]int

	byKey      map[string]payment.CreatePaymentResult
	byEventKey map[string]payment.Event
}

var _ payment.Provider = (*MemProvider)(nil)

// NewMemProvider создаёт двойник провайдера с именем name. Имя задаётся только
// здесь: по нему сервис узнаёт провайдера в каждом событии.
func NewMemProvider(name payment.ProviderName) *MemProvider {
	return &MemProvider{
		name:       name,
		payments:   map[string]payment.Event{},
		rejectFor:  map[string]bool{},
		failFor:    map[string]bool{},
		calls:      map[string]int{},
		byKey:      map[string]payment.CreatePaymentResult{},
		byEventKey: map[string]payment.Event{},
	}
}

// Name — имя провайдера; задано при сборке и не меняется, поэтому без замка.
func (p *MemProvider) Name() payment.ProviderName { return p.name }

// SetCreateErr и соседние SetGetErr, SetCaptureErr, SetCancelErr, SetRefundErr,
// SetParseErr — сбой внешнего мира на КАЖДОМ вызове метода; nil снимает сбой.
func (p *MemProvider) SetCreateErr(err error) { p.set(func() { p.createErr = err }) }

func (p *MemProvider) SetGetErr(err error) { p.set(func() { p.getErr = err }) }

func (p *MemProvider) SetCaptureErr(err error) { p.set(func() { p.captureErr = err }) }

func (p *MemProvider) SetCancelErr(err error) { p.set(func() { p.cancelErr = err }) }

func (p *MemProvider) SetRefundErr(err error) { p.set(func() { p.refundErr = err }) }

func (p *MemProvider) SetParseErr(err error) { p.set(func() { p.parseErr = err }) }

// SetNoHolds — двухстадийной оплаты нет: Capture и Cancel отвечают
// ErrUnsupported, как адаптер провайдера без холдов.
func (p *MemProvider) SetNoHolds(v bool) { p.set(func() { p.noHolds = v }) }

// SetBadSignature — ParseWebhook вернёт ErrInvalidSignature, НЕ разбирая тело.
func (p *MemProvider) SetBadSignature(v bool) { p.set(func() { p.badSignature = v }) }

// SetNoPaymentID — ответить «принял», но платёж не назвать: так проверяется,
// что сервис не переводит намерение в pending без id, о котором потом можно
// спросить сверку.
func (p *MemProvider) SetNoPaymentID(v bool) { p.set(func() { p.noPaymentID = v }) }

// SetResult — шаблон ответа на первое создание платежа.
func (p *MemProvider) SetResult(res payment.CreatePaymentResult) { p.set(func() { p.result = res }) }

// SetRefundEcho — Refund ответит ЭТОЙ суммой вместо запрошенной; ноль снимает.
// Так изображается провайдер, вернувший не то, о чём просили: домен обязан
// отказаться записывать в книгу цифру, которой не было.
func (p *MemProvider) SetRefundEcho(amountMinor int64) { p.set(func() { p.refundEcho = amountMinor }) }

// RejectReference — по этой ссылке потребителя провайдер отказывает
// ДЕТЕРМИНИРОВАННО: платёж не создастся, и повтор даст тот же отказ.
func (p *MemProvider) RejectReference(reference string) {
	p.set(func() { p.rejectFor[reference] = true })
}

// RejectNext — отказ достанется следующему НОВОМУ платежу, какой бы ни была его
// ссылка: HTTP-тест потребителя её заранее не знает, она рождается внутри
// ручки. Повтор по ключу отдаёт то, что уже было; вызовы копятся.
func (p *MemProvider) RejectNext() { p.set(func() { p.rejectNext++ }) }

// FailReference — по этой ссылке провайдер не отвечает: временный сбой, ретрай
// осмыслен.
func (p *MemProvider) FailReference(reference string) {
	p.set(func() { p.failFor[reference] = true })
}

// SetCreateHook — хук внутри CreatePayment, до ответа: так тест изображает
// событие, приехавшее РОВНО между вставкой намерения и ответом провайдера.
// Зовётся ВНЕ замка и вправе трогать сам провайдер.
func (p *MemProvider) SetCreateHook(hook func(req payment.CreatePaymentRequest)) {
	p.set(func() { p.createHook = hook })
}

// Created и соседние Captures, Cancels, Refunds — запросы к провайдеру по порядку;
// копии, а не своя память двойника.
func (p *MemProvider) Created() []payment.CreatePaymentRequest { return cloneLocked(p, &p.created) }

func (p *MemProvider) Captures() []payment.CaptureRequest { return cloneLocked(p, &p.captures) }

func (p *MemProvider) Cancels() []string { return cloneLocked(p, &p.cancels) }

func (p *MemProvider) Refunds() []payment.RefundProviderRequest { return cloneLocked(p, &p.refunds) }

// set — мутация ручки под тем же замком, под которым её читают методы.
func (p *MemProvider) set(mutate func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mutate()
}

// cloneLocked — копия среза двойника, снятая под его замком.
func cloneLocked[T any](p *MemProvider, s *[]T) []T {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(*s)
}

// CreatePayment создаёт платёж; идемпотентен по req.IdempotencyKey.
func (p *MemProvider) CreatePayment(_ context.Context, req payment.CreatePaymentRequest,
) (payment.CreatePaymentResult, error) {
	// Хук зовётся ВНЕ замка: он изображает событие, приехавшее между вставкой
	// намерения и ответом провайдера, и вправе трогать сам провайдер — под
	// замком это была бы взаимная блокировка.
	if hook := p.recordCreate(req); hook != nil {
		hook(req)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return payment.CreatePaymentResult{}, p.createErr
	}
	if p.failFor[req.Reference] {
		return payment.CreatePaymentResult{}, fmt.Errorf("%w: reference %q", ErrProviderDown, req.Reference)
	}
	if prev, ok := p.byKey[req.IdempotencyKey]; ok {
		return prev, nil
	}

	res := p.result
	if p.rejectFor[req.Reference] || p.takeRejectNext() {
		res = payment.CreatePaymentResult{Status: payment.EventFailed}
	}
	if res.Status == "" {
		res.Status = payment.EventPending
	}
	if res.Status != payment.EventFailed && res.ProviderPaymentID == "" && !p.noPaymentID {
		res.ProviderPaymentID = "pay-" + req.IntentID.String()
		res.Confirmation = payment.Confirmation{
			Type: payment.ConfirmationRedirect,
			URL:  "https://pay.example/" + res.ProviderPaymentID,
		}
	}
	p.byKey[req.IdempotencyKey] = res
	if res.ProviderPaymentID != "" {
		p.payments[res.ProviderPaymentID] = p.event(res.ProviderPaymentID, req.IntentID,
			payment.EventPending, req.AmountMinor, req.Currency)
	}
	return res, nil
}

// recordCreate запоминает запрос и отдаёт хук — под замком, чтобы сам хук звать
// уже без него.
func (p *MemProvider) recordCreate(req payment.CreatePaymentRequest) func(payment.CreatePaymentRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls["CreatePayment"]++
	p.created = append(p.created, req)
	return p.createHook
}

// takeRejectNext тратит один отказ «следующему»; зовётся под замком.
func (p *MemProvider) takeRejectNext() bool {
	if p.rejectNext == 0 {
		return false
	}
	p.rejectNext--
	return true
}

// ParseWebhook отдаёт следующее событие из очереди (Push).
func (p *MemProvider) ParseWebhook(_ context.Context, _ payment.WebhookRequest,
) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls["ParseWebhook"]++
	// Подлинность проверяется ДО разбора тела и до любого похода в БД — двойник
	// повторяет этот порядок, иначе тест «неподтверждённый вебхук не стоит нам
	// ни одного обращения к стору» проверял бы не то.
	if p.badSignature {
		return payment.Event{}, payment.ErrInvalidSignature
	}
	if p.parseErr != nil {
		return payment.Event{}, p.parseErr
	}
	if len(p.inbox) == 0 {
		return payment.Event{}, ErrNoEvent
	}
	ev := p.inbox[0]
	p.inbox = p.inbox[1:]
	return ev, nil
}

// GetPayment — состояние платежа у провайдера.
func (p *MemProvider) GetPayment(_ context.Context, providerPaymentID string) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls["GetPayment"]++
	if p.getErr != nil {
		return payment.Event{}, p.getErr
	}
	ev, ok := p.payments[providerPaymentID]
	if !ok {
		return payment.Event{}, ErrNoPayment
	}
	return ev, nil
}

// Capture списывает холд; идемпотентен по ключу. При SetNoHolds —
// ErrUnsupported.
func (p *MemProvider) Capture(_ context.Context, req payment.CaptureRequest) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls["Capture"]++
	p.captures = append(p.captures, req)
	if p.noHolds {
		return payment.Event{}, fmt.Errorf("%w: two-stage payments", payment.ErrUnsupported)
	}
	if p.captureErr != nil {
		return payment.Event{}, p.captureErr
	}
	return p.settle(req.IdempotencyKey, req.ProviderPaymentID, payment.EventSucceeded,
		req.AmountMinor, req.Currency), nil
}

// Cancel снимает холд; идемпотентен по ключу. При SetNoHolds — ErrUnsupported.
func (p *MemProvider) Cancel(_ context.Context, providerPaymentID, idempotencyKey string,
) (payment.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls["Cancel"]++
	p.cancels = append(p.cancels, providerPaymentID)
	if p.noHolds {
		return payment.Event{}, fmt.Errorf("%w: two-stage payments", payment.ErrUnsupported)
	}
	if p.cancelErr != nil {
		return payment.Event{}, p.cancelErr
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
	p.calls["Refund"]++
	p.refunds = append(p.refunds, req)
	if p.refundErr != nil {
		return payment.Event{}, p.refundErr
	}
	if prev, ok := p.byEventKey[req.IdempotencyKey]; ok {
		return prev, nil
	}
	echoed := req.AmountMinor
	if p.refundEcho != 0 {
		echoed = p.refundEcho
	}
	ev := p.event(req.ProviderPaymentID, uuid.Nil, payment.EventRefunded,
		echoed, req.Currency)
	// У возврата свой id события: он не должен совпасть с id зачисления, иначе
	// дедуп проглотил бы одно как дубль другого.
	ev.ProviderEventID = fmt.Sprintf("%s:refund:%s:%d",
		p.name, req.ProviderPaymentID, req.AmountMinor)
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
	intentID := p.payments[paymentID].IntentID
	if amountMinor == 0 && currency == "" {
		amountMinor, currency = p.payments[paymentID].AmountMinor, p.payments[paymentID].Currency
	}
	ev := p.event(paymentID, intentID, t, amountMinor, currency)
	p.byEventKey[key] = ev
	p.payments[paymentID] = ev
	return ev
}

// SetPayment кладёт состояние платежа, которое вернёт GetPayment.
func (p *MemProvider) SetPayment(providerPaymentID string, ev payment.Event) {
	p.set(func() { p.payments[providerPaymentID] = ev })
}

// Push кладёт событие в очередь ParseWebhook.
func (p *MemProvider) Push(ev payment.Event) {
	p.set(func() { p.inbox = append(p.inbox, ev) })
}

// CallCount — сколько раз звали метод; через мьютекс, потому что двойник живёт
// и под конкурентными тестами.
func (p *MemProvider) CallCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method]
}

// RefundKeys — различные ключи идемпотентности, с которыми звали Refund. По их
// числу тест считает, сколько РАЗНЫХ возвратов увидел бы провайдер.
func (p *MemProvider) RefundKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.refunds))
	seen := map[string]bool{}
	for _, r := range p.refunds {
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
		Provider:          p.name,
		ProviderEventID:   fmt.Sprintf("%s:%s:%s", p.name, providerPaymentID, t),
		ProviderPaymentID: providerPaymentID,
		IntentID:          intentID,
		Type:              t,
		AmountMinor:       amountMinor,
		Currency:          currency,
	}
}
