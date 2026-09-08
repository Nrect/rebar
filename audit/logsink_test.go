package audit_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
)

// logged — одна строка JSON-лога, разобранная в карту.
func logged(t *testing.T, write func(sink *audit.LogSink)) map[string]any {
	t.Helper()
	var buf strings.Builder
	write(audit.NewLogSink(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "событие — ровно одна строка лога")
	out := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &out))
	return out
}

// Nil-логгер — паника: приёмник с ним молча съедал бы журнал.
func TestNewLogSink_PanicsOnNilLogger(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "audit.NewLogSink: logger must not be nil", func() {
		audit.NewLogSink(nil)
	})
}

func TestLogSink_WritesAllFields(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
		e.Outcome = audit.OutcomeDenied
		e.Details = map[string]string{"reason": "bad_password", "attempt": "3"}
	}))
	require.NoError(t, err)

	line := logged(t, func(sink *audit.LogSink) {
		assert.NoError(t, sink.Write(t.Context(), ev))
	})

	assert.Equal(t, audit.LogMessage, line["msg"])
	assert.Equal(t, slog.LevelInfo.String(), line["level"])
	assert.Equal(t, ev.ID.String(), line["audit.id"])
	assert.Equal(t, "user.login", line["audit.action"])
	assert.Equal(t, "denied", line["audit.outcome"])
	assert.Equal(t, "user", line["audit.actor_kind"])
	assert.Equal(t, "u-1", line["audit.actor_id"])
	assert.Equal(t, "teacher@school.ru", line["audit.actor_name"])
	assert.Equal(t, "user", line["audit.target_type"])
	assert.Equal(t, "req-1", line["audit.request_id"])
	assert.Equal(t, "203.0.113.7", line["audit.ip"])

	details, ok := line["audit.details"].(map[string]any)
	require.True(t, ok, "подробности — группа, а не плоские ключи")
	assert.Equal(t, "bad_password", details["reason"])
	assert.Equal(t, "3", details["attempt"])
}

// Пустые необязательные поля в строку не идут: пустой ключ в лог-хранилище —
// это лишний столбец, который потом отличают от отсутствующего.
func TestLogSink_OmitsEmptyFields(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorAnonymous})
	ev, err := rec.Prepare(ctx, audit.Entry{Action: actionLogin, Outcome: audit.OutcomeDenied})
	require.NoError(t, err)

	line := logged(t, func(sink *audit.LogSink) {
		assert.NoError(t, sink.Write(t.Context(), ev))
	})

	assert.Equal(t, "anonymous", line["audit.actor_kind"])
	for _, key := range []string{
		"audit.actor_id", "audit.actor_name", "audit.target_type",
		"audit.target_id", "audit.request_id", "audit.ip", "audit.details",
	} {
		assert.NotContains(t, line, key)
	}
}

// Уровень один на все исходы: подними порог у потребителя — и часть журнала
// исчезла бы вместе с ним.
func TestLogSink_SameLevelForEveryOutcome(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	for _, outcome := range audit.AllOutcomes {
		ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Outcome = outcome }))
		require.NoError(t, err)

		line := logged(t, func(sink *audit.LogSink) {
			assert.NoError(t, sink.Write(t.Context(), ev))
		})
		assert.Equal(t, slog.LevelInfo.String(), line["level"], "исход %q", outcome)
	}
}

// Событие с nil-подробностями приёмник переживает: адаптер их переживает тоже.
func TestLogSink_SurvivesZeroEvent(t *testing.T) {
	t.Parallel()

	line := logged(t, func(sink *audit.LogSink) {
		assert.NoError(t, sink.Write(t.Context(), audit.Event{}))
	})
	assert.Equal(t, audit.LogMessage, line["msg"])
}

// Пустая группа подробностей до строки лога не доходит: slog опускает её сам,
// поэтому условие в приёмнике экономит сборку атрибутов, а не меняет вывод
// (mutants_internal_test.go, разбор эквивалентных мутантов).
func TestLogSink_EmptyDetailsGroupIsElided(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	empty, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Details = map[string]string{} }))
	require.NoError(t, err)

	line := logged(t, func(sink *audit.LogSink) {
		assert.NoError(t, sink.Write(t.Context(), empty))
	})
	assert.NotContains(t, line, "audit.details")
}
