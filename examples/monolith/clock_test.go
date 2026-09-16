package monolith_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/examples/monolith"
)

// nil-часы падают на настройке, а не разыменованием nil в ручке.
func TestApp_SetClockPanicsOnNil(t *testing.T) {
	assert.PanicsWithValue(t, "monolith.App.SetClock: now must not be nil",
		func() { new(monolith.App).SetClock(nil) })
}

// TestClock_HandlersTakeAppClock — заказ, событие без occurred_at и загрузка
// пишут момент часов приложения. Часы стоят в 2031 году: time.Now() в ручке с
// ними не сойдётся, а сверка — Equal, а не «примерно сейчас».
func TestClock_HandlersTakeAppClock(t *testing.T) {
	s := newStand(t)
	now := s.app.Now()
	require.True(t, inUTC(now), "часы по умолчанию — не time.UTC, а %q", now.Location())

	// 789 нс сверх микросекунды отличают усечение от округления.
	moment := time.Date(2031, time.March, 9, 2, 30, 15, 123456789, time.UTC)
	s.app.SetClock(func() time.Time { return moment })

	signIn(t, s, registerAndConfirm(t, s))
	intent, order := checkout(t, s)
	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "вебхук: %s", raw(body))
	status, body = s.upload(t, "clock.png", pngBody())
	require.Equal(t, http.StatusCreated, status, "загрузка: %s", raw(body))

	requireStoredMoment(t, s, moment, "SELECT created_at FROM shop_orders WHERE id = $1", order)
	requireStoredMoment(t, s, moment, "SELECT occurred_at FROM payment_events WHERE intent_id = $1", intent)
	requireStoredMoment(t, s, moment, "SELECT created_at FROM shop_uploads WHERE object_key = $1", str(t, body, "key"))
}

// inUTC — пояс сверяется указателем, а не именем: time.Local при TZ=UTC тоже
// называется «UTC».
func inUTC(moment time.Time) bool { return moment.Location() == time.UTC }

// requireStoredMoment — момент в базе равен want так, как его хранит
// timestamptz: то же мгновение до микросекунд. Зону база не хранит, а pgx отдаёт
// строку в поясе процесса, поэтому сообщение — в UTC.
func requireStoredMoment(t *testing.T, s *stand, want time.Time, query string, args ...any) {
	t.Helper()
	var got time.Time
	require.NoError(t, s.pool(t).QueryRow(t.Context(), query, args...).Scan(&got), "запрос: %s", query)
	want = want.UTC().Truncate(time.Microsecond)
	require.Truef(t, got.Equal(want), "%s: в базе %s, часы %s",
		query, got.UTC().Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
}
