package mailtest_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// Настройка правится на ходу: тест потребителя меняет её, пока ручка его
// HTTP-сервера в другой горутине шлёт письма. Под -race это обязано быть чисто.
func TestTransport_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	transport := mailtest.NewTransport()
	env := envelope("teacher", storeBase)
	whileServing(
		func() { // ручка сервера: Send на адрес без отказа читает обе карты
			for range 200 {
				_, _ = transport.Send(context.Background(), env)
			}
		},
		func(int) { transport.RejectFor("other@school.ru", "MessageRejected") },
		func(i int) { transport.FailFor(env.To.Email, i%2) },
		func(i int) {
			if i%2 == 0 {
				transport.SetSendHook(func(context.Context, mail.Envelope) (mail.SendResult, error) {
					return mail.SendResult{ProviderMessageID: "hook"}, nil
				})
				return
			}
			transport.SetSendHook(nil)
		},
		func(int) { _ = transport.Sent() },
	)
}

// Хук зовётся вне замка: хук, позвавший сам двойник, иначе повесил бы Send.
func TestTransport_SendHookMayCallTheTransport(t *testing.T) {
	t.Parallel()

	transport := mailtest.NewTransport()
	transport.SetSendHook(func(context.Context, mail.Envelope) (mail.SendResult, error) {
		transport.FailFor("other@school.ru", 1)
		_ = transport.Sent()
		return mail.SendResult{ProviderMessageID: "hook"}, nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := transport.Send(context.Background(), envelope("teacher", storeBase))
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("хук, позвавший двойник, повесил Send: он зовётся под замком")
	}

	transport.SetSendHook(nil)
	_, err := transport.Send(context.Background(), envelope("other", storeBase))
	require.ErrorIs(t, err, mailtest.ErrSendFailed, "настройка из хука применилась")
}

// Счётчик FailFor правится и тратится параллельно. Запись мимо замка здесь — не
// красный тест, а fatal error: concurrent map writes.
func TestTransport_FailForSurvivesParallelUse(t *testing.T) {
	t.Parallel()
	const workers, rounds = 8, 300

	transport := mailtest.NewTransport()
	env := envelope("teacher", storeBase)
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				transport.FailFor(env.To.Email, i%3)
				if _, err := transport.Send(context.Background(), env); err != nil {
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, workers*rounds, int(failed.Load())+len(transport.Sent()),
		"каждая отправка либо записана, либо провалена, и ровно одно из двух")
}
