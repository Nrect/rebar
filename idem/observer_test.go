package idem_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

// Уровень — по тому, кому нужен человек: дефект ручки — Error, дефект
// клиента — Warn, остальное — Debug. В записи только операция и исход.
func TestLogObserver_LevelsAndAttrs(t *testing.T) {
	t.Parallel()

	wantLevel := map[idem.Outcome]string{
		idem.OutcomeExecuted: "DEBUG", idem.OutcomeReplayed: "DEBUG", idem.OutcomeReused: "WARN",
		idem.OutcomeInFlight: "DEBUG", idem.OutcomeFailed: "DEBUG", idem.OutcomeNotRecordable: "ERROR",
		idem.OutcomeTooLarge: "ERROR", idem.OutcomeError: "DEBUG",
	}
	require.Len(t, wantLevel, len(idem.AllOutcomes), "каждый исход с уровнем")

	for _, outcome := range idem.AllOutcomes {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		idem.LogObserver(logger).Outcome(t.Context(), opCreate, outcome)

		var entry map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "одна запись JSON на исход %s", outcome)
		delete(entry, "time")
		assert.Equal(t, map[string]any{
			"level": wantLevel[outcome], "msg": "idem outcome", "op": string(opCreate), "outcome": string(outcome),
		}, entry, "исход %s", outcome)
	}
}

func TestLogObserver_NilLoggerIsDefault(t *testing.T) {
	t.Parallel()

	obs := idem.LogObserver(nil)
	require.NotNil(t, obs)
	assert.NotPanics(t, func() { obs.Outcome(t.Context(), opCreate, idem.OutcomeExecuted) })
}

// Watch у лога — пустой ход: заводить нулём нечего, а строка на сборку
// хранилища была бы шумом.
func TestLogObserver_WatchWritesNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	idem.LogObserver(logger).Watch(opCreate)
	assert.Empty(t, buf.String())
}
