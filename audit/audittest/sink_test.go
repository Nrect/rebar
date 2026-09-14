package audittest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/audittest"
)

func event(action audit.Action, outcome audit.Outcome) audit.Event {
	return audit.Event{
		ID:      uuid.New(),
		Action:  action,
		Outcome: outcome,
		Actor:   audit.Actor{Kind: audit.ActorUser, ID: "u-1"},
		Details: map[string]string{"reason": "тест"},
	}
}

// Двойник моделирует append-only: порядок записи сохраняется, а метода
// очистки нет — свежий журнал это NewSink.
func TestSink_KeepsWriteOrder(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	require.NoError(t, sink.Write(t.Context(), event("a", audit.OutcomeSuccess)))
	require.NoError(t, sink.Write(t.Context(), event("b", audit.OutcomeDenied)))
	require.NoError(t, sink.Write(t.Context(), event("a", audit.OutcomeFailure)))

	events := sink.Events()
	require.Len(t, events, 3)
	assert.Equal(t, audit.Action("a"), events[0].Action)
	assert.Equal(t, audit.Action("b"), events[1].Action)
	assert.Equal(t, audit.Action("a"), events[2].Action)
	assert.Equal(t, 3, sink.Count())

	byAction := sink.ByAction("a")
	require.Len(t, byAction, 2)
	assert.Equal(t, audit.OutcomeSuccess, byAction[0].Outcome)
	assert.Equal(t, audit.OutcomeFailure, byAction[1].Outcome)
}

// Записанное не меняется ни правкой карты вызывающего, ни правкой того, что
// отдали читателю: журнал — история.
func TestSink_WrittenEventsAreImmutable(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	ev := event("a", audit.OutcomeSuccess)
	require.NoError(t, sink.Write(t.Context(), ev))

	ev.Details["reason"] = "подмена после записи"
	sink.Events()[0].Details["reason"] = "подмена через читателя"

	stored, ok := sink.Last()
	require.True(t, ok)
	assert.Equal(t, "тест", stored.Details["reason"])
}

// Ошибка двойника отличима от доменной и записи не оставляет.
func TestSink_ErrStopsWrite(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	sink.SetErr(audittest.ErrSinkFailed)

	err := sink.Write(t.Context(), event("a", audit.OutcomeSuccess))
	require.ErrorIs(t, err, audittest.ErrSinkFailed)
	require.NotErrorIs(t, err, audit.ErrUnavailable, "поломка стенда отличима от доменного отказа")
	assert.Zero(t, sink.Count())
}

// МОМЕНТ ХРАНИТСЯ ТАК, КАК ЕГО ХРАНИТ timestamptz У auditpg: в UTC и до
// микросекунд. Двойник, отдающий наносекунды и чужую зону, зеленит у
// потребителя сравнение меток, которое на базе красное. То же утверждение
// держит адаптер: auditpg, TestSink_Write_StoresMomentAsTimestamptz.
func TestSink_KeepsMomentAsTimestamptz(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	ev := event("a", audit.OutcomeSuccess)
	ev.At = time.Date(2026, 9, 14, 12, 0, 0, 123456789, time.FixedZone("UTC+3", 3*60*60))
	require.NoError(t, sink.Write(t.Context(), ev))

	stored, ok := sink.Last()
	require.True(t, ok)
	assert.Truef(t, sameStoredMoment(stored.At, ev.At), "момент %s, ожидалось %s — в UTC и до микросекунд",
		stored.At.Format(time.RFC3339Nano), ev.At.Truncate(time.Microsecond).UTC().Format(time.RFC3339Nano))
}

// sameStoredMoment — got равен want так, как want хранит timestamptz:
// усечённым до микросекунд и в UTC. Голый Equal зоны не видит.
func sameStoredMoment(got, want time.Time) bool {
	return got.Location() == time.UTC && got.Equal(want.Truncate(time.Microsecond))
}

// Пустой журнал не паникует и последнего события не выдумывает.
func TestSink_EmptyIsSafe(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	_, ok := sink.Last()
	assert.False(t, ok)
	assert.Empty(t, sink.Events())
	assert.Empty(t, sink.ByAction("a"))
	assert.Zero(t, sink.Count())
}

// Пограничные аргументы двойник переживает так же, как адаптер: нулевое
// событие с nil-подробностями его не роняет.
func TestSink_SurvivesZeroEvent(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	require.NoError(t, sink.Write(t.Context(), audit.Event{}))

	stored, ok := sink.Last()
	require.True(t, ok)
	assert.Nil(t, stored.Details, "nil остаётся nil: двойник не приукрашивает данные")
}

// Двойник потокобезопасен: тесты потребителя идут под -race.
func TestSink_Race(t *testing.T) {
	t.Parallel()

	const writers = 8
	const perWriter = 50

	sink := audittest.NewSink()
	var wg sync.WaitGroup
	wg.Add(writers * 2)
	for range writers {
		go func() {
			defer wg.Done()
			for range perWriter {
				_ = sink.Write(t.Context(), event("a", audit.OutcomeSuccess))
			}
		}()
		go func() {
			defer wg.Done()
			for range perWriter {
				_ = sink.Events()
				_, _ = sink.Last()
				_ = sink.Count()
				_ = sink.ByAction("a")
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, writers*perWriter, sink.Count())
}
