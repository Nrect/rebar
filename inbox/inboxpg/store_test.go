package inboxpg_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
	"github.com/nrect/rebar/postgres"
)

// Ошибка сборки падает на старте. Пул ленивый: паника раньше первого соединения.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(t.Context(), "postgres://inboxpg@127.0.0.1:1/none")
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	for _, tc := range []struct {
		want string
		call func()
	}{
		{"inboxpg.New: nil pool", func() { inboxpg.New(nil, only(nop)) }},
		{"inboxpg.New: at least one source handler is required", func() { inboxpg.New(pool, nil) }},
		{`inboxpg.New: source "Billing" must match [a-z0-9_]{1,32}`, func() {
			inboxpg.New(pool, map[inbox.SourceName]inboxpg.Handler{"Billing": nop})
		}},
		{`inboxpg.New: handler of source "billing" must not be nil`, func() { inboxpg.New(pool, only(nil)) }},
		{"inboxpg.WithTx: nil tx", func() { inboxpg.New(pool, only(nop)).WithTx(nil) }},
	} {
		assert.PanicsWithValue(t, tc.want, tc.call)
	}
}

// Карта обработчиков копируется: правка вызывающим после New источники не меняет.
func TestNew_CopiesHandlers(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(t.Context(), "postgres://inboxpg@127.0.0.1:1/none")
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	handlers := only(nop)
	store := inboxpg.New(pool, handlers)
	handlers["acme"] = nop
	delete(handlers, billing)
	assert.Equal(t, []inbox.SourceName{billing}, store.Sources())
}

// Отметка, тело и эффект обработчика — одна транзакция (решение 6): после отказа
// из базы не читается ни одна из трёх строк, и законный повтор применяется.
// Отказ приходит тремя путями: ошибкой обработчика, ошибкой базы, которую
// обработчик проглотил, и сбоем самого COMMIT — отложенный триггер на таблице
// эффекта падает после всех шагов Accept, и его текст в ошибке это доказывает.
func TestStore_Accept_IsAtomic(t *testing.T) {
	t.Parallel()

	errRefused := errors.New("inboxpg_test: handler refused")
	store, pool := newStore(t, only(handlerFunc(func(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
		if err := recordEffect(ctx, tx, ev); err != nil {
			return err
		}
		switch string(ev.Payload) {
		case "refuse":
			return errRefused
		case "swallow":
			_, _ = tx.Exec(ctx, `SELECT 1/0`)
		}
		return nil
	})))
	shopTables(t, pool)
	_, err := pool.Exec(t.Context(), `
		CREATE FUNCTION shop_effects_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'inboxpg_test: commit refused';
		END $$;
		CREATE CONSTRAINT TRIGGER shop_effects_refuse_trg AFTER INSERT ON shop_effects
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
			WHEN (NEW.event_id = 'evt-atomic-commit') EXECUTE FUNCTION shop_effects_refuse()`)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, id, payload string
		want              error
		text              string
		// repair — что снять, чтобы повтор прошёл.
		repair string
	}{
		{name: "ошибка обработчика", id: "evt-atomic-refused", payload: "refuse", want: errRefused},
		{name: "проглоченная ошибка базы", id: "evt-atomic-swallowed", payload: "swallow", want: inbox.ErrUnavailable},
		{
			name: "сбой коммита", id: "evt-atomic-commit", payload: "commit", want: inbox.ErrUnavailable, text: "commit refused",
			repair: `DROP TRIGGER shop_effects_refuse_trg ON shop_effects`,
		},
	} {
		ev := testEvent(tc.id, tc.payload)
		_, err = store.Accept(t.Context(), ev, testNow)
		require.ErrorIs(t, err, tc.want, tc.name)
		assert.Contains(t, err.Error(), tc.text, tc.name)
		assert.Equal(t, [3]int{}, stored(t, pool, ev.ID), "%s: отметка, тело или эффект пережили откат", tc.name)

		if tc.repair != "" {
			_, err = pool.Exec(t.Context(), tc.repair)
			require.NoError(t, err)
		}
		ev = testEvent(tc.id, "retry")
		mustAccept(t, store, ev, inbox.OutcomeAccepted)
		assert.Equal(t, [3]int{1, 1, 1}, stored(t, pool, ev.ID), "%s: повтор не применился", tc.name)
	}
}

// N параллельных доставок одного события — эффект ровно один (решение 14):
// принята одна, остальные — in_flight или duplicate, а отметка, тело и эффект
// читаются из базы по одной строке. Обработчик держит ключ дольше, чем соседи
// идут до блокировки.
func TestStore_Accept_Race(t *testing.T) {
	t.Parallel()

	const deliveries = 24
	store, pool := newStore(t, only(handlerFunc(func(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
		if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.05)`); err != nil {
			return err
		}
		return recordEffect(ctx, tx, ev)
	})))
	shopTables(t, pool)
	ev := testEvent("evt-race", "race")

	start := make(chan struct{})
	outcomes := make(chan inbox.Outcome, deliveries)
	var wg sync.WaitGroup
	for range deliveries {
		wg.Go(func() {
			<-start
			outcome, err := store.Accept(context.Background(), ev, testNow)
			if err != nil {
				t.Errorf("параллельная доставка: %v", err)
			}
			outcomes <- outcome
		})
	}
	close(start)
	wg.Wait()
	close(outcomes)

	counts := map[inbox.Outcome]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	assert.Equal(t, 1, counts[inbox.OutcomeAccepted], "принятых из параллельных доставок: %v", counts)
	assert.Equal(t, deliveries-1, counts[inbox.OutcomeInFlight]+counts[inbox.OutcomeDuplicate],
		"остальные — in_flight или duplicate: %v", counts)
	assert.Equal(t, [3]int{1, 1, 1}, stored(t, pool, ev.ID), "отметка, тело и эффект — по одной строке")
	mustAccept(t, store, ev, inbox.OutcomeDuplicate)
}

// Accept держит ровно золотой ключ: TestLockKey_Golden сторожит формулу, этот —
// проводку. Ключ bigint лежит в pg_locks старшими 32 битами в classid и
// младшими в objid, objsubid = 1.
func TestStore_Accept_LocksGoldenKey(t *testing.T) {
	t.Parallel()

	golden := int64(-390798276003009787) // ("billing", "evt_2"), посчитан вне Go
	var held int
	store, _ := newStore(t, only(handlerFunc(func(ctx context.Context, tx pgx.Tx, _ inbox.Event) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND pid = pg_backend_pid() AND classid = $1 AND objid = $2 AND objsubid = 1`,
			uint32(uint64(golden)>>32), uint32(uint64(golden))).Scan(&held)
	})))
	mustAccept(t, store, testEvent("evt_2", "golden"), inbox.OutcomeAccepted)
	assert.Equal(t, 1, held, "Accept взял не золотой ключ")
}

// Пустое тело — пустое, а не NULL: двойник событие без тела принимает, и адаптер обязан.
func TestStore_Accept_EmptyPayload(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	ev := testEvent("evt-empty", "")
	ev.Payload = nil
	mustAccept(t, store, ev, inbox.OutcomeAccepted)

	payload, ok, err := reader{pool: pool}.Payload(t.Context(), billing, ev.ID)
	require.NoError(t, err)
	assert.True(t, ok, "тела нет")
	assert.Empty(t, payload)
}

// Тело не переживает отметку: уборка отметки при живом теле уносит его каскадом,
// а в счёт идёт одна отметка — каскад не считается.
func TestStore_Purge_MarkTakesPayload(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	ev := testEvent("evt-cascade", "cascade")
	ev.OccurredAt = testNow.Add(-3 * time.Hour)
	outcome, err := store.Accept(t.Context(), ev, testNow.Add(-2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, inbox.OutcomeAccepted, outcome)

	// Отметка старше своей границы, тело моложе своей.
	deleted, err := store.Purge(t.Context(), testNow.Add(-time.Hour), testNow.Add(-3*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "каскад в счёт не идёт")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM inbox_events`), "отметка осталась")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM inbox_payloads`), "тело пережило отметку")
}

// Равные моменты уходят по ключу, как у двойника: уборка детерминирована.
// Вставка обратным порядком — иначе физический порядок строк совпал бы с ключом.
func TestStore_Purge_EqualMomentsByKey(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	for _, id := range []string{"evt-tie-b", "evt-tie-a"} {
		ev := testEvent(id, id)
		ev.OccurredAt = testNow.Add(-2 * time.Hour)
		outcome, err := store.Accept(t.Context(), ev, testNow.Add(-time.Hour))
		require.NoError(t, err)
		require.Equal(t, inbox.OutcomeAccepted, outcome)
	}

	deleted, err := store.Purge(t.Context(), testNow, testNow, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, deleted, "тело и отметка одного события")
	r := reader{pool: pool}
	_, gone, err := r.Mark(t.Context(), billing, "evt-tie-a")
	require.NoError(t, err)
	_, kept, err := r.Mark(t.Context(), billing, "evt-tie-b")
	require.NoError(t, err)
	assert.True(t, !gone && kept, "первым уходит меньший ключ: evt-tie-a убран %t, evt-tie-b цел %t", !gone, kept)
}

// secretBody — «персональные данные» тела. В начале значения: Detail режет
// каждое поле до 64 символов, а bytea печатает hex.
const secretBody = "SECRET-7f3a-body"

// В Detail Postgres кладёт «Failing row contains (…)» — тело события. Наружу оно
// не уходит: граница — тип *postgres.Error, а *pgconn.PgError через неё не
// проходит. Контроль мимо адаптера доказывает, что утекать есть чему.
func TestStore_Error_DoesNotLeakPayload(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	body := append([]byte(secretBody), make([]byte, inbox.MaxPayloadBytes)...)
	hexBody := hex.EncodeToString([]byte(secretBody))

	_, err := pool.Exec(t.Context(), `INSERT INTO inbox_events (source, event_id, event_type, digest, occurred_at, received_at)
		VALUES ('billing', 'evt-leak-raw', 'invoice.paid', $1, $2, $2)`, make([]byte, inbox.DigestSize), testNow)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO inbox_payloads (source, event_id, payload, received_at)
		VALUES ('billing', 'evt-leak-raw', $1, $2)`, body, testNow)
	var raw *pgconn.PgError
	require.ErrorAs(t, err, &raw)
	require.Contains(t, raw.Detail, hexBody, "контроль: иначе тест ниже ничего не доказывает")

	ev := testEvent("evt-leak", "")
	sum := sha256.Sum256(body)
	ev.Payload, ev.Digest = body, sum[:]
	_, err = store.Accept(t.Context(), ev, testNow)
	require.ErrorIs(t, err, inbox.ErrUnavailable)
	for _, leak := range []string{secretBody, hexBody, "Failing row"} {
		assert.NotContains(t, err.Error(), leak)
	}

	var pgErr *pgconn.PgError
	assert.NotErrorAs(t, err, &pgErr, "*pgconn.PgError не уезжает наружу")
	var clean *postgres.Error
	require.ErrorAs(t, err, &clean, "наружу едет очищенная ошибка postgres")
	assert.Equal(t, "23514", clean.Code)
	assert.Equal(t, "inbox_payloads_size_chk", clean.Constraint)
}
