package payment_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// providerPath — одно из пяти мест вызова провайдера: готовит состояние, ставит
// сбой нужного метода и зовёт операцию.
type providerPath struct {
	name string
	run  func(t *testing.T, h *harness, fail error) (payment.Reason, error)
}

func providerPaths() []providerPath {
	return []providerPath{
		{"start", func(t *testing.T, h *harness, fail error) (payment.Reason, error) {
			t.Helper()
			h.prov.CreateErr = fail
			_, reason, err := h.svc.Start(t.Context(), startReq())
			return reason, err
		}},
		{"capture", func(t *testing.T, h *harness, fail error) (payment.Reason, error) {
			t.Helper()
			in := h.hold(t)
			h.prov.CaptureErr = fail
			_, reason, err := h.svc.Capture(t.Context(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
			return reason, err
		}},
		{"cancel", func(t *testing.T, h *harness, fail error) (payment.Reason, error) {
			t.Helper()
			in := h.hold(t)
			h.prov.CancelErr = fail
			_, reason, err := h.svc.Cancel(t.Context(), in.ID, "cancel-1")
			return reason, err
		}},
		{"refund", func(t *testing.T, h *harness, fail error) (payment.Reason, error) {
			t.Helper()
			in := h.sold(t)
			h.prov.RefundErr = fail
			_, reason, err := h.svc.Refund(t.Context(), payment.RefundRequest{
				IntentID: in.ID, AmountMinor: 100, IdempotencyKey: "refund-1",
				ActorID: uuid.New(), Receipt: receiptFor(100),
			})
			return reason, err
		}},
		{"reconcile", func(t *testing.T, h *harness, fail error) (payment.Reason, error) {
			t.Helper()
			in := h.start(t, startReq())
			h.prov.GetErr = fail
			return h.svc.Reconcile(t.Context(), in.ID)
		}},
	}
}

// Окончательный отказ провайдера — не «попробуйте позже»: под ErrUnavailable
// потребитель отдал бы на него 503, и клиент повторял бы вечно то, что ретраем
// не чинится никогда. «Ответа нет» — наоборот, только ErrUnavailable. Класс
// обязан выйти одинаково из всех пяти мест вызова.
func TestProviderError_ClassSurvivesEveryPath(t *testing.T) {
	t.Parallel()

	classes := []struct {
		name        string
		fail        error
		class       error
		reason      payment.Reason
		unavailable bool
	}{
		{"отказ по существу", fmt.Errorf("adapter: 422: %w", payment.ErrProviderRejected),
			payment.ErrProviderRejected, payment.ReasonProviderRejected, false},
		{"операции у адаптера нет", fmt.Errorf("adapter: %w", payment.ErrUnsupported),
			payment.ErrUnsupported, payment.ReasonUnsupported, false},
		{"сеть", paymenttest.ErrProviderDown,
			paymenttest.ErrProviderDown, payment.ReasonProviderError, true},
	}

	for _, path := range providerPaths() {
		for _, tc := range classes {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				reason, err := path.run(t, newHarness(t), tc.fail)

				require.ErrorIs(t, err, tc.class)
				assert.Equal(t, tc.reason, reason)
				if tc.unavailable {
					require.ErrorIs(t, err, payment.ErrUnavailable)
				} else {
					require.NotErrorIs(t, err, payment.ErrUnavailable)
				}
			})
		}
	}
}

// ErrProviderRejected ошибкой из CreatePayment — тот же определённый «не
// создал», что Status == EventFailed: попытка закрывается, повтор ключа отдаёт
// тот же отказ, а деньги, если провайдер их всё же пришлёт, не зачисляются.
func TestStart_ProviderRejectedError_ClosesTheAttempt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.CreateErr = fmt.Errorf("adapter: 422: %w", payment.ErrProviderRejected)
	req := startReq()

	res, reason, err := h.svc.Start(t.Context(), req)

	require.ErrorIs(t, err, payment.ErrProviderRejected)
	require.NotErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderRejected, reason)
	assert.Equal(t, payment.StatusFailed, res.Intent.Status)

	h.prov.CreateErr = nil
	_, reason, err = h.svc.Start(t.Context(), req)
	require.ErrorIs(t, err, payment.ErrProviderRejected, "повтор ключа — тот же отказ")
	assert.Equal(t, payment.ReasonProviderRejected, reason)
	assert.Equal(t, 1, h.prov.CallCount("CreatePayment"), "к провайдеру повторно не ходим")

	h.prov.Push(h.prov.Event("pay-late", res.Intent.ID, payment.EventSucceeded, testAmount, "RUB"))
	_, reason, err = h.svc.HandleWebhook(t.Context(), webhook())

	require.NoError(t, err, "провайдеру 200, человеку алерт")
	assert.Equal(t, payment.ReasonStatusConflict, reason)
	assert.Empty(t, h.store.EntriesOf(res.Intent.ID, payment.LedgerCapture), "закрытое намерение зачисления не принимает")
	assert.Equal(t, payment.StatusFailed, h.mustIntent(t, res.Intent.ID).Status)
}

// Сверка брошенной попытки получает тот же определённый отказ и закрывает её
// тем же путём, что Start: второй таблицы соответствий нет.
func TestReconcile_UnstartedProviderRejected_ClosesTheAttempt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *payment.Config) { c.RequireReceipt = false })
	in := h.unstarted(t)
	h.prov.CreateErr = fmt.Errorf("adapter: 422: %w", payment.ErrProviderRejected)

	reason, err := h.svc.Reconcile(t.Context(), in.ID)

	require.ErrorIs(t, err, payment.ErrProviderRejected)
	require.NotErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderRejected, reason)
	assert.Equal(t, payment.StatusFailed, h.mustIntent(t, in.ID).Status)
}
