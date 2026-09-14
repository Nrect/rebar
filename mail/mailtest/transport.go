package mailtest

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/nrect/rebar/mail"
)

// TransportName — имя двойника; не совпадает с mail.UnconfiguredName, иначе
// Deliver не тронул бы очередь.
const TransportName mail.TransportName = "mem"

// ErrSendFailed — временный сбой двойника. Не *mail.RejectedError: Deliver
// обязан назначить повтор, а не увести строку в failed.
var ErrSendFailed = errors.New("mailtest: transport is temporarily unavailable")

// Transport — записывающий mail.Transport: конверты складываются, отказы и
// сбои задаются по адресу получателя методами. Потокобезопасен целиком,
// включая настройку.
//
// Публичных полей нет: Send читает настройку под замком, а тест потребителя
// правит её, пока ручка его HTTP-сервера в другой горутине шлёт письмо
// (CONVENTIONS §3).
type Transport struct {
	mu   sync.Mutex
	sent []mail.Envelope

	rejectFor map[string]string
	failFor   map[string]int
	sendHook  func(ctx context.Context, env mail.Envelope) (mail.SendResult, error)
}

// NewTransport — двойник без отказов.
func NewTransport() *Transport {
	return &Transport{rejectFor: map[string]string{}, failFor: map[string]int{}}
}

// Name — TransportName.
func (t *Transport) Name() mail.TransportName { return TransportName }

// RejectFor — постоянный отказ провайдера письмам на email: *mail.RejectedError
// с кодом code.
func (t *Transport) RejectFor(email, code string) { t.set(func() { t.rejectFor[email] = code }) }

// FailFor — ближайшие times отправок на email провалить временным сбоем.
// Заменяет прежний счётчик; ноль снимает.
func (t *Transport) FailFor(email string, times int) { t.set(func() { t.failFor[email] = times }) }

// SetSendHook — если задан, отвечает вместо всего остального: так тест
// проверяет таймаут (подождать ctx.Done) или порядок вызовов; nil снимает.
// Зовётся вне замка и вправе звать сам двойник.
func (t *Transport) SetSendHook(hook func(ctx context.Context, env mail.Envelope) (mail.SendResult, error)) {
	t.set(func() { t.sendHook = hook })
}

// set — правка настройки под тем же замком, под которым её читает Send.
func (t *Transport) set(mutate func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mutate()
}

// Send записывает конверт и возвращает непустой ProviderMessageID.
func (t *Transport) Send(ctx context.Context, env mail.Envelope) (mail.SendResult, error) {
	t.mu.Lock()
	hook := t.sendHook
	t.mu.Unlock()
	// Хук зовётся без замка: он вправе ждать ctx.Done и звать сам двойник.
	if hook != nil {
		return hook(ctx, env)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if code, rejected := t.rejectFor[env.To.Email]; rejected {
		return mail.SendResult{}, &mail.RejectedError{Code: code, Reason: "mailtest: recipient is rejected"}
	}
	if left := t.failFor[env.To.Email]; left > 0 {
		t.failFor[env.To.Email] = left - 1
		return mail.SendResult{}, ErrSendFailed
	}
	t.sent = append(t.sent, copyEnvelope(env))
	return mail.SendResult{ProviderMessageID: "mem-" + uuid.NewString()}, nil
}

// Sent — копии принятых конвертов в порядке приёма.
func (t *Transport) Sent() []mail.Envelope {
	t.mu.Lock()
	defer t.mu.Unlock()
	sent := make([]mail.Envelope, len(t.sent))
	for i, env := range t.sent {
		sent[i] = copyEnvelope(env)
	}
	return sent
}
