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
// сбои задаются по адресу получателя. Потокобезопасен.
type Transport struct {
	mu   sync.Mutex
	sent []mail.Envelope

	// RejectFor — email получателя → код: постоянный отказ провайдера.
	RejectFor map[string]string
	// FailFor — email → сколько ближайших Send провалить временным сбоем.
	FailFor map[string]int
	// SendHook — если задан, отвечает вместо всего остального: так тест
	// проверяет таймаут (подождать ctx.Done) или порядок вызовов.
	SendHook func(ctx context.Context, env mail.Envelope) (mail.SendResult, error)
}

// NewTransport — двойник без отказов.
func NewTransport() *Transport {
	return &Transport{RejectFor: map[string]string{}, FailFor: map[string]int{}}
}

// Name — TransportName.
func (t *Transport) Name() mail.TransportName { return TransportName }

// Send записывает конверт и возвращает непустой ProviderMessageID.
func (t *Transport) Send(ctx context.Context, env mail.Envelope) (mail.SendResult, error) {
	t.mu.Lock()
	hook := t.SendHook
	t.mu.Unlock()
	// Хук зовётся без замка: он вправе ждать ctx.Done, а двойник — отвечать другим.
	if hook != nil {
		return hook(ctx, env)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if code, rejected := t.RejectFor[env.To.Email]; rejected {
		return mail.SendResult{}, &mail.RejectedError{Code: code, Reason: "mailtest: recipient is rejected"}
	}
	if left := t.FailFor[env.To.Email]; left > 0 {
		t.FailFor[env.To.Email] = left - 1
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
