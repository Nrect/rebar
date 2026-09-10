package paymentotel_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
	"github.com/nrect/rebar/payment/paymenttest"
)

// serviceEnv — сервис ядра на двойниках с декоратором и наблюдателем на одном
// метре: метрика проверяется против того, что сервис отдал вызывающему.
type serviceEnv struct {
	svc    *payment.Service
	prov   *paymenttest.MemProvider
	reader *sdkmetric.ManualReader
}

func newServiceEnv(t *testing.T) serviceEnv {
	t.Helper()
	reader, meter := newMeter(t)
	prov := paymenttest.NewMemProvider("memprov")
	p, err := paymentotel.Wrap(prov, meter)
	require.NoError(t, err)
	obs, err := paymentotel.NewObserver(meter)
	require.NoError(t, err)
	svc := payment.NewService(paymenttest.NewMemStore(), p, obs, serviceConfig())
	svc.SetClock(paymenttest.NewClock(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)).Now)
	return serviceEnv{svc: svc, prov: prov, reader: reader}
}

// pending — намерение, платёж по которому у провайдера создан.
func (e serviceEnv) pending(t *testing.T) payment.Intent {
	t.Helper()
	res, _, err := e.svc.Start(t.Context(), startRequest("order-1"))
	require.NoError(t, err)
	return res.Intent
}

// deliver — событие провайдера про намерение, доставленное вебхуком.
func (e serviceEnv) deliver(t *testing.T, in payment.Intent, typ payment.EventType) payment.Intent {
	t.Helper()
	e.prov.Push(e.prov.Event(in.ProviderPaymentID, in.ID, typ, in.AmountMinor, in.Currency))
	res, _, err := e.svc.HandleWebhook(t.Context(), payment.WebhookRequest{Raw: []byte(`{}`)})
	require.NoError(t, err)
	return res.Intent
}

// servicePath — операция сервиса, которая ходит к провайдеру ровно одним
// вызовом call, и сбой fail приходит именно на нём.
type servicePath struct {
	call paymentotel.CallType
	run  func(t *testing.T, e serviceEnv, fail error) error
}

func servicePaths() []servicePath {
	return []servicePath{
		{paymentotel.CallCreatePayment, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			e.prov.CreateErr = fail
			_, _, err := e.svc.Start(t.Context(), startRequest("order-1"))
			return err
		}},
		{paymentotel.CallParseWebhook, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			e.prov.ParseErr = fail
			_, _, err := e.svc.HandleWebhook(t.Context(), payment.WebhookRequest{Raw: []byte(`{}`)})
			return err
		}},
		{paymentotel.CallGetPayment, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			in := e.pending(t)
			e.prov.GetErr = fail
			_, err := e.svc.Reconcile(t.Context(), in.ID)
			return err
		}},
		{paymentotel.CallCapture, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			in := e.deliver(t, e.pending(t), payment.EventAuthorized)
			e.prov.CaptureErr = fail
			_, _, err := e.svc.Capture(t.Context(), in.ID, in.AmountMinor, nil, "cap-1")
			return err
		}},
		{paymentotel.CallCancel, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			in := e.deliver(t, e.pending(t), payment.EventAuthorized)
			e.prov.CancelErr = fail
			_, _, err := e.svc.Cancel(t.Context(), in.ID, "cancel-1")
			return err
		}},
		{paymentotel.CallRefund, func(t *testing.T, e serviceEnv, fail error) error {
			t.Helper()
			in := e.deliver(t, e.pending(t), payment.EventSucceeded)
			e.prov.RefundErr = fail
			_, _, err := e.svc.Refund(t.Context(), payment.RefundRequest{
				IntentID: in.ID, AmountMinor: 100, IdempotencyKey: "refund-1", ActorID: uuid.New(),
			})
			return err
		}},
	}
}

// Метрика считает то, что делает сервис: result=error ровно тогда, когда
// сервис вернул ErrUnavailable (503, повтор), и rejected — когда вернул
// окончательный отказ. Разойдись они — алерт «провайдер недоступен» врал бы на
// тех самых ошибках, на которых потребитель отвечает 503.
func TestWrap_ResultAgreesWithService(t *testing.T) {
	t.Parallel()
	classes := map[string]error{
		"отказ по существу":          payment.ErrProviderRejected,
		"операции нет":               payment.ErrUnsupported,
		"подпись не сошлась":         payment.ErrInvalidSignature,
		"мусор":                      payment.ErrMalformedEvent,
		"недоступен":                 payment.ErrUnavailable,
		"сырой таймаут":              context.DeadlineExceeded,
		"мусор поверх недоступности": fmt.Errorf("%w: %w", payment.ErrMalformedEvent, payment.ErrUnavailable),
	}
	for _, path := range servicePaths() {
		for name, fail := range classes {
			t.Run(string(path.call)+"/"+name, func(t *testing.T) {
				t.Parallel()
				e := newServiceEnv(t)

				err := path.run(t, e, fail)

				require.Error(t, err)
				want := paymentotel.ResultRejected
				if errors.Is(err, payment.ErrUnavailable) {
					want = paymentotel.ResultError
				}
				assert.Equal(t, int64(1), callCount(t, collect(t, e.reader), path.call, want),
					"сервис ответил: %v", err)
			})
		}
	}
}
