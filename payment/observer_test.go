package payment_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Набор op закрыт: на этих строках стоят алерты потребителя, и смена любой из
// них ломающая (VERSIONING).
func TestAllOps_IsClosedSet(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []payment.Op{"start", "webhook", "capture", "cancel", "refund", "reconcile"}, payment.AllOps)
}

// Каждая публичная операция отдаёт наблюдателю РОВНО ОДИН исход — тот самый
// Reason, что вернула вызывающему, на успехе и на ошибке. Разойдись они — алерт
// по метке считал бы не то, что видит транспорт.
func TestObserver_EveryOperationReportsItsReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		op      payment.Op
		prepare func(t *testing.T, h *harness) func() payment.Reason
	}{
		{"start: создано", payment.OpStart, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			return func() payment.Reason {
				_, reason, _ := h.svc.Start(t.Context(), startReq())
				return reason
			}
		}},
		{"start: ключ негоден", payment.OpStart, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			req := startReq()
			req.IdempotencyKey = " "
			return func() payment.Reason {
				_, reason, _ := h.svc.Start(t.Context(), req)
				return reason
			}
		}},
		{"webhook: зачислено", payment.OpWebhook, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			in := h.start(t, startReq())
			h.prov.Push(h.event(in, payment.EventSucceeded, in.AmountMinor))
			return func() payment.Reason {
				_, reason, _ := h.svc.HandleWebhook(t.Context(), webhook())
				return reason
			}
		}},
		{"webhook: подпись не сошлась", payment.OpWebhook, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			h.prov.SetBadSignature(true)
			return func() payment.Reason {
				_, reason, _ := h.svc.HandleWebhook(t.Context(), webhook())
				return reason
			}
		}},
		{"capture: списано", payment.OpCapture, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			in := h.hold(t)
			return func() payment.Reason {
				_, reason, _ := h.svc.Capture(t.Context(), in.ID, in.AmountMinor, receiptFor(testAmount), "cap-1")
				return reason
			}
		}},
		{"capture: намерения нет", payment.OpCapture, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			return func() payment.Reason {
				_, reason, _ := h.svc.Capture(t.Context(), uuid.New(), testAmount, receiptFor(testAmount), "cap-1")
				return reason
			}
		}},
		{"cancel: холд снят", payment.OpCancel, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			in := h.hold(t)
			return func() payment.Reason {
				_, reason, _ := h.svc.Cancel(t.Context(), in.ID, "cancel-1")
				return reason
			}
		}},
		{"cancel: намерения нет", payment.OpCancel, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			return func() payment.Reason {
				_, reason, _ := h.svc.Cancel(t.Context(), uuid.New(), "cancel-1")
				return reason
			}
		}},
		{"refund: возвращено", payment.OpRefund, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			in := h.sold(t)
			return func() payment.Reason {
				_, reason, _ := h.svc.Refund(t.Context(), payment.RefundRequest{
					IntentID: in.ID, AmountMinor: 100, IdempotencyKey: "refund-1",
					ActorID: uuid.New(), Receipt: receiptFor(100),
				})
				return reason
			}
		}},
		{"refund: без автора", payment.OpRefund, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			return func() payment.Reason {
				_, reason, _ := h.svc.Refund(t.Context(), payment.RefundRequest{
					IntentID: uuid.New(), AmountMinor: 100, IdempotencyKey: "refund-1",
				})
				return reason
			}
		}},
		{"reconcile: платёж жив", payment.OpReconcile, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			in := h.start(t, startReq())
			return func() payment.Reason {
				reason, _ := h.svc.Reconcile(t.Context(), in.ID)
				return reason
			}
		}},
		{"reconcile: намерения нет", payment.OpReconcile, func(t *testing.T, h *harness) func() payment.Reason {
			t.Helper()
			return func() payment.Reason {
				reason, _ := h.svc.Reconcile(t.Context(), uuid.New())
				return reason
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			do := tc.prepare(t, h)
			before := len(h.obs.Outcomes())

			reason := do()

			got := h.obs.Outcomes()[before:]
			require.Len(t, got, 1, "исход на вызов — ровно один")
			assert.Equal(t, paymenttest.Observed{Op: tc.op, Reason: reason}, got[0])
		})
	}
}

// Сверка пачкой — это Reconcile на каждое намерение: исходов столько же, сколько
// разобранных намерений.
func TestObserver_ReconcilerReportsEveryIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.startQueue(t, 3)
	h.clock.Advance(h.cfg.StalePendingAfter + time.Second)
	before := len(h.obs.Outcomes())

	done, err := payment.NewReconciler(h.svc, 10).Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 3, done)
	got := h.obs.Outcomes()[before:]
	require.Len(t, got, 3)
	for _, o := range got {
		assert.Equal(t, paymenttest.Observed{Op: payment.OpReconcile, Reason: payment.ReasonStillPending}, o)
	}
}

// Без метрик тревоги с порогом 1 не должны пропасть: LogObserver пишет их в
// Error, остальное — в Debug, и в записи только операция с причиной.
func TestLogObserver_MoneyAlertsAreErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	obs := payment.LogObserver(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	obs.Outcome(t.Context(), payment.OpWebhook, payment.ReasonStatusConflict)
	obs.Outcome(t.Context(), payment.OpWebhook, payment.ReasonAmountMismatch)
	obs.Outcome(t.Context(), payment.OpStart, payment.ReasonCreated)

	var records []map[string]any
	for dec := json.NewDecoder(&buf); dec.More(); {
		var rec map[string]any
		require.NoError(t, dec.Decode(&rec))
		records = append(records, rec)
	}
	want := []struct{ level, op, reason string }{
		{"ERROR", "webhook", "status_conflict"},
		{"ERROR", "webhook", "amount_mismatch"},
		{"DEBUG", "start", "created"},
	}
	require.Len(t, records, len(want))
	for i, w := range want {
		assert.Equal(t, w.level, records[i]["level"])
		assert.Equal(t, w.op, records[i]["op"])
		assert.Equal(t, w.reason, records[i]["reason"])
		assert.Len(t, records[i], 5, "time, level, msg, op, reason — и ничего больше")
	}
}

func TestLogObserver_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		payment.LogObserver(nil).Outcome(t.Context(), payment.OpStart, payment.ReasonCreated)
	})
}
