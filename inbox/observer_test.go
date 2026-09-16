package inbox_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
)

// Уровень — по тому, кто и когда нужен: conflict и too_large ждут человека,
// сигналы с выдержкой — Warn, штатное — Debug. В записи только закрытые наборы.
func TestLogObserver_Levels(t *testing.T) {
	t.Parallel()

	want := map[inbox.Outcome]string{
		inbox.OutcomeConflict: "ERROR", inbox.OutcomeTooLarge: "ERROR",
		inbox.OutcomeNotAuthentic: "WARN", inbox.OutcomeUnknownType: "WARN", inbox.OutcomeMalformed: "WARN", inbox.OutcomeError: "WARN",
		inbox.OutcomeAccepted: "DEBUG", inbox.OutcomeDuplicate: "DEBUG", inbox.OutcomeIgnored: "DEBUG", inbox.OutcomeInFlight: "DEBUG",
	}
	require.Len(t, want, len(inbox.AllOutcomes), "уровень назван каждому исходу")
	for _, outcome := range inbox.AllOutcomes {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		inbox.LogObserver(logger).Received(t.Context(), billing, outcome, 1500*time.Millisecond)

		var record map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &record), outcome)
		delete(record, "time")
		assert.Equal(t, map[string]any{
			"level": want[outcome], "msg": "inbox received",
			"source": "billing", "outcome": string(outcome), "took": float64(1500 * time.Millisecond),
		}, record, outcome)
	}
}

// Через сервис в лог не уходит ни тело, ни подпись, ни ключ события.
func TestLogObserver_ThroughServiceLogsNoDelivery(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := billingStore()
	svc := inbox.NewService(store, inbox.LogObserver(logger), testConfig(stubVerifier()))
	svc.SetClock(func() time.Time { return start })

	req := signed("evt_secret_key_42", typePaid, map[string]string{"phone": "+79990001122"})
	for _, delivery := range []inbox.Request{req, req, inboxtest.SignHMAC([]byte("forged"), start, req.Raw)} {
		_, _ = svc.Receive(t.Context(), billing, delivery)
	}
	logged := buf.String()
	assert.Equal(t, 3, strings.Count(logged, `"msg":"inbox received"`))
	for _, secretPart := range []string{"evt_secret_key_42", "+79990001122", req.Headers[inboxtest.SignatureHeader][0]} {
		assert.NotContains(t, logged, secretPart)
	}
}

func TestLogObserver_NilLoggerUsesDefault(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		inbox.LogObserver(nil).Received(t.Context(), billing, inbox.OutcomeAccepted, time.Millisecond)
	})
}
