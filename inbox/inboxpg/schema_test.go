package inboxpg_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
)

// UPDATE отметки и тела отбивает база, а не только отсутствие запроса в адаптере:
// отказ приходит именем inbox_append_only — его узнают по ConstraintName, не по
// тексту, — и строки не меняются.
func TestSchema_RefusesUpdateByName(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	ev := testEvent("evt-update", "update")
	mustAccept(t, store, ev, inbox.OutcomeAccepted)

	for _, tc := range []struct {
		name, sql string
		arg       any
	}{
		{"отметка", `UPDATE inbox_events SET event_type = $1 WHERE event_id = 'evt-update'`, "invoice.voided"},
		{"тело", `UPDATE inbox_payloads SET payload = $1 WHERE event_id = 'evt-update'`, []byte("rewritten")},
	} {
		_, err := pool.Exec(t.Context(), tc.sql, tc.arg)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "%s: UPDATE обязан быть отвергнут", tc.name)
		assert.Equal(t, "23514", pgErr.Code, tc.name)
		assert.Equal(t, "inbox_append_only", pgErr.ConstraintName, tc.name)
	}

	mark, ok, err := reader{pool: pool}.Mark(t.Context(), billing, ev.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, typePaid, mark.Type, "отметка изменилась")
	payload, _, err := reader{pool: pool}.Payload(t.Context(), billing, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, ev.Payload, payload, "тело изменилось")
}

// DELETE база пропускает: удалить тело по сроку — обязанность перед законом о
// персональных данных (решение 5). Удалённая отметка уносит тело каскадом.
func TestSchema_AllowsDelete(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	mustAccept(t, store, testEvent("evt-delete-payload", "payload"), inbox.OutcomeAccepted)
	mustAccept(t, store, testEvent("evt-delete-mark", "mark"), inbox.OutcomeAccepted)

	_, err := pool.Exec(t.Context(), `DELETE FROM inbox_payloads WHERE event_id = 'evt-delete-payload'`)
	require.NoError(t, err, "DELETE тела")
	_, err = pool.Exec(t.Context(), `DELETE FROM inbox_events WHERE event_id = 'evt-delete-mark'`)
	require.NoError(t, err, "DELETE отметки")

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM inbox_events WHERE event_id = 'evt-delete-payload'`),
		"без тела отметка живёт")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM inbox_payloads`), "тело пережило удаление")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM inbox_events WHERE event_id = 'evt-delete-mark'`))
}

// Оба триггера — ENABLE ALWAYS сразу после наката: в режиме ORIGIN они молчат
// при репликации, то есть там, где строки и правят руками.
func TestSchema_TriggersAlwaysEnabled(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t, only(nop))
	assert.Equal(t, map[string]string{
		"inbox_events_append_only_trg":   "A",
		"inbox_payloads_append_only_trg": "A",
	}, triggerModes(t, pool))
}

// CHECK схемы — ровно формы ядра: каждый ASCII-байт в источнике, ключе и типе
// база принимает тогда и только тогда, когда его принимает валидатор inbox, и
// отказывает именем своего CHECK; потолки длины те же.
func TestSchema_ChecksMirrorCoreForms(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t, only(nop))
	insert := func(source, id, typ string) error {
		_, err := pool.Exec(t.Context(), `INSERT INTO inbox_events (source, event_id, event_type, digest, occurred_at, received_at)
			VALUES ($1, $2, $3, $4, $5, $5)`, source, id, typ, make([]byte, inbox.DigestSize), testNow)
		return err
	}
	type field struct {
		name, check string
		valid       func(string) bool
		insert      func(value string, n int) error
	}
	fields := []field{
		{"источник", "inbox_events_source_chk", func(s string) bool { return inbox.SourceName(s).Valid() },
			func(v string, n int) error { return insert(v, fmt.Sprintf("evt-source-%d", n), "t") }},
		{"ключ", "inbox_events_id_chk", inbox.ValidEventID,
			func(v string, _ int) error { return insert("forms_id", v, "t") }},
		{"тип", "inbox_events_type_chk", func(s string) bool { return inbox.EventType(s).Valid() },
			func(v string, n int) error { return insert("forms_type", fmt.Sprintf("evt-type-%d", n), v) }},
	}
	for _, f := range fields {
		values := []string{
			strings.Repeat("a", inbox.MaxSourceLen), strings.Repeat("b", inbox.MaxSourceLen+1),
			strings.Repeat("c", inbox.MaxEventTypeLen), strings.Repeat("d", inbox.MaxEventTypeLen+1),
			strings.Repeat("e", inbox.MaxEventIDLen), strings.Repeat("f", inbox.MaxEventIDLen+1),
		}
		for c := byte(1); c < 0x80; c++ {
			values = append(values, string([]byte{c}))
		}
		for n, v := range values {
			err := f.insert(v, n)
			if f.valid(v) {
				assert.NoError(t, err, "%s %q: форма ядра годна, база отказала", f.name, v)
				continue
			}
			var pgErr *pgconn.PgError
			if assert.ErrorAs(t, err, &pgErr, "%s %q: форма ядра негодна, база приняла", f.name, v) {
				assert.Equal(t, f.check, pgErr.ConstraintName, "%s %q: отказ не своим CHECK", f.name, v)
			}
		}
	}
}

// triggerModes — tgenabled триггеров inbox прямо из pg_trigger, мимо
// CheckSchema: страж не опирается на проверяемый код.
func triggerModes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT tgname, tgenabled::text FROM pg_trigger
		WHERE tgrelid IN ('inbox_events'::regclass, 'inbox_payloads'::regclass) AND NOT tgisinternal`)
	require.NoError(t, err)
	defer rows.Close()
	modes := map[string]string{}
	for rows.Next() {
		var name, mode string
		require.NoError(t, rows.Scan(&name, &mode))
		modes[name] = mode
	}
	require.NoError(t, rows.Err())
	return modes
}
