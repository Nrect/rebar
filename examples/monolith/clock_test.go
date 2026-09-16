package monolith_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementpg"

	"github.com/nrect/rebar/examples/monolith"
)

// nil-часы падают на настройке, а не разыменованием nil в ручке.
func TestApp_SetClockPanicsOnNil(t *testing.T) {
	assert.PanicsWithValue(t, "monolith.App.SetClock: now must not be nil",
		func() { new(monolith.App).SetClock(nil) })
}

// После Start часы не подменяются: задачи уже читают их из своих горутин.
func TestApp_SetClockAfterStartPanics(t *testing.T) {
	s := newStand(t)
	startProcess(t, s)
	assert.PanicsWithValue(t, "monolith.App.SetClock: called after Start",
		func() { s.app.SetClock(func() time.Time { return time.Time{} }) })
}

// TestClock_HandlersTakeAppClock — ручки и блоки пишут момент часов приложения:
// App.SetClock отдаёт те же часы каждому блоку со своими часами. Часы стоят в
// 2031 году: time.Now() где угодно с ними не сойдётся, а сверка — Equal, а не
// «примерно сейчас».
func TestClock_HandlersTakeAppClock(t *testing.T) {
	s := newStand(t)
	now := s.app.Now()
	require.True(t, inUTC(now), "часы по умолчанию — не time.UTC, а %q", now.Location())

	// 789 нс сверх микросекунды отличают усечение от округления.
	moment := time.Date(2031, time.March, 9, 2, 30, 15, 123456789, time.UTC)
	s.app.SetClock(func() time.Time { return moment })

	subject := registerAndConfirm(t, s)
	signIn(t, s, subject)
	intent, order := checkout(t, s)
	status, body := s.postJSON(t, "/webhook", providerBody(intent))
	require.Equal(t, http.StatusOK, status, "вебхук: %s", raw(body))
	require.Equal(t, 1, s.runJob(t, "outbox_drain"), "событие об оплате разобрано")
	status, body = s.upload(t, "clock.png", pngBody())
	require.Equal(t, http.StatusCreated, status, "загрузка: %s", raw(body))
	key := str(t, body, "key")

	for _, m := range []struct {
		query string
		args  []any
	}{
		// Ручки монолита.
		{"SELECT created_at FROM shop_orders WHERE id = $1", []any{order}},
		{"SELECT occurred_at FROM payment_events WHERE intent_id = $1", []any{intent}},
		{"SELECT created_at FROM shop_uploads WHERE object_key = $1", []any{key}},
		// session.
		{"SELECT created_at FROM auth_tokens WHERE subject_id = $1 AND purpose = 'verify'", []any{subject}},
		{"SELECT created_at FROM auth_sessions WHERE subject_id = $1", []any{subject}},
		// mail.
		{"SELECT created_at FROM email_outbox WHERE kind = 'verify'", nil},
		{"SELECT sent_at FROM email_outbox WHERE kind = 'verify'", nil},
		// audit.
		{"SELECT occurred_at FROM audit_events WHERE action = 'auth.signed_in'", nil},
		// payment; выдача берёт момент записи книги.
		{"SELECT created_at FROM payment_intents WHERE id = $1", []any{intent}},
		{"SELECT received_at FROM payment_events WHERE intent_id = $1", []any{intent}},
		{"SELECT created_at FROM payment_ledger WHERE intent_id = $1 AND kind = 'capture'", []any{intent}},
		{"SELECT min(granted_at) FROM entitlement_grants WHERE subject_id = $1", []any{subject}},
		{"SELECT max(granted_at) FROM entitlement_grants WHERE subject_id = $1", []any{subject}},
		// outbox: Producer и Worker.
		{"SELECT created_at FROM outbox_messages WHERE kind = 'order.paid'", nil},
		{"SELECT done_at FROM outbox_messages WHERE kind = 'order.paid'", nil},
	} {
		assertStoredMoment(t, s, moment, m.query, m.args...)
	}

	// scheduler: штамп последнего успеха — из часов прогона.
	requireMetric(t, s.scrape(t), "cron_last_success_timestamp_seconds",
		map[string]string{"job": "outbox_drain"}, float64(moment.UnixNano())/float64(time.Second))

	// objectstore.Collector: объект без строки — сирота, и по часам 2031 года он
	// старше MinAge, хотя записан минуту назад.
	_, err := s.pool(t).Exec(t.Context(), "DELETE FROM shop_uploads WHERE object_key = $1", key)
	require.NoError(t, err)
	require.Equal(t, 1, s.runJob(t, "objectstore_collect"), "сирота старше MinAge по часам приложения")
}

// TestClock_ExpiryByAppClock — сроки выдачи и роли сверяются часами приложения:
// их получили entitlement.Service и authzpg.Store. Срок — посередине между
// настоящим временем и часами 2031 года: по часам приложения он истёк, по
// настоящим нет, и от минуты прогона тест не зависит.
func TestClock_ExpiryByAppClock(t *testing.T) {
	s := newStand(t)
	moment := time.Date(2031, time.March, 9, 2, 30, 15, 123456789, time.UTC)
	s.app.SetClock(func() time.Time { return moment })
	subject := registerAndConfirm(t, s)
	signIn(t, s, subject)

	realNow := time.Now().UTC()
	until := realNow.Add(moment.Sub(realNow) / 2)

	// entitlement.Service: запрос к lesson-03 — первый для субъекта, и снимок
	// грузится на нём же, так что TTL снимка не спасает ни одну сторону.
	require.NoError(t, entitlementpg.New(s.pool(t)).Grant(t.Context(), subject,
		entitlement.Grant{ItemID: "lesson-03", ExpiresAt: &until}, realNow))
	status, body := s.get(t, "/lesson/lesson-03")
	require.Equal(t, http.StatusForbidden, status, "entitlement.Service: выдача lesson-03 до %s на часах %s не истекла: %s",
		until.Format(time.RFC3339), moment.Format(time.RFC3339Nano), raw(body))
	require.Equal(t, "item-not-open", body["slug"], "отказ по предмету, а не по праву")

	// authzpg.Store: Assign переписывает срок бессрочной роли стенда, и
	// покупатель до until — единственная роль субъекта.
	require.NoError(t, authzpg.New(s.pool(t)).Assign(t.Context(), authzpg.Assignment{
		Subject: authz.Subject{Realm: "shop", ID: subject.String()}, Role: "customer",
		GrantedBy: "test", GrantedAt: realNow, ExpiresAt: &until,
	}))
	status, body = s.upload(t, "clock.png", pngBody())
	require.Equal(t, http.StatusForbidden, status, "authzpg.Store: роль customer до %s на часах %s не истекла: %s",
		until.Format(time.RFC3339), moment.Format(time.RFC3339Nano), raw(body))
	require.Equal(t, "forbidden", body["slug"], "отказ по праву")
}

// inUTC — пояс сверяется указателем, а не именем: time.Local при TZ=UTC тоже
// называется «UTC».
func inUTC(moment time.Time) bool { return moment.Location() == time.UTC }

// assertStoredMoment — момент в базе равен want так, как его хранит
// timestamptz: то же мгновение до микросекунд. Зону база не хранит, а pgx отдаёт
// строку в поясе процесса, поэтому сообщение — в UTC. Расхождение — assert:
// проба видит все непереданные часы разом.
func assertStoredMoment(t *testing.T, s *stand, want time.Time, query string, args ...any) {
	t.Helper()
	var got time.Time
	require.NoError(t, s.pool(t).QueryRow(t.Context(), query, args...).Scan(&got), "запрос: %s", query)
	want = want.UTC().Truncate(time.Microsecond)
	assert.Truef(t, got.Equal(want), "%s: в базе %s, часы %s",
		query, got.UTC().Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
}
