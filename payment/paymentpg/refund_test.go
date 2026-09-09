package paymentpg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
)

// Двухстадийная оплата целиком: холд, списание с записью в книгу, два частичных
// возврата. Статус возвратом не меняется — возврат это встречная запись в
// append-only книге, а не мутация статуса; иначе succeeded перестал бы быть
// терминальным и запоздалое событие воскресило бы оплату после возврата.
func TestStore_Hold_CaptureRefund(t *testing.T) {
	t.Parallel()

	store, pool, hook := hookedStore(t, nil)
	in := mustCreate(t, store, intent(func(in *payment.Intent) { in.AutoCapture = false }))
	toPending(t, store, in)

	now := testNow()
	held := event(in, payment.EventAuthorized)
	res, err := store.ApplyEvent(t.Context(), applyRequest(in, held, payment.StatusAuthorized, nil, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Nil(t, res.Intent.SettledAt, "холд — ещё не зачисление")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_ledger`),
		"переход без денег книгу не трогает")

	paid := event(in, payment.EventSucceeded)
	capture := captureEntry(in, paid, now)
	res, err = store.ApplyEvent(t.Context(), applyRequest(in, paid, payment.StatusSucceeded, capture, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)

	// Возвраты происходят позже зачисления и в разные моменты: порядок книги —
	// это порядок времени, и тест обязан его различать.
	first := refundEntry(in, *capture, 30000, "refund:1", now.Add(time.Second))
	refunded, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: first, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeApplied, refunded.Outcome)

	second := refundEntry(in, *capture, in.AmountMinor-30000, "refund:2", now.Add(2*time.Second))
	refunded, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: second, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeApplied, refunded.Outcome, "частичных возвратов бывает несколько")

	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, payment.LedgerCapture, entries[0].Kind, "порядок по возрастанию created_at")
	assert.Equal(t, first.ID, entries[1].ID)
	assert.Equal(t, second.ID, entries[2].ID)
	net, err := payment.Net(entries, in.Currency)
	require.NoError(t, err)
	assert.True(t, net.IsZero(), "после полного возврата нетто ровно ноль")

	after, _, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Equal(t, payment.StatusSucceeded, after.Status, "возврат статуса не меняет")

	// Автор ручного движения денег и ссылка на зачисление сохранены: без них
	// вопрос «кто вернул эти деньги» остался бы без ответа навсегда.
	require.NotNil(t, entries[1].ActorID)
	assert.Equal(t, *first.ActorID, *entries[1].ActorID)
	require.NotNil(t, entries[1].ReversesEntryID)
	assert.Equal(t, capture.ID, *entries[1].ReversesEntryID)

	settled, refundedCalls := hook.calls()
	assert.Equal(t, 1, settled)
	assert.Equal(t, 2, refundedCalls, "хук зовётся на каждом возврате")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM shop_orders WHERE kind = 'refunded'`))
}

// Потолок Σrefund ≤ Σcapture держат два независимых рубежа. Первый — блокировка
// намерения: два частичных возврата, поданных одновременно, не могут оба влезть.
// Второй — триггер книги: он ловит запись в обход сервиса, для которой никакой
// блокировки в Go не существует.
func TestStore_ApplyRefund_SecondOverCaptureIsRejected(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	capture := settleIntent(t, store, in)
	now := testNow()

	// Половина суммы плюс половина суммы плюс копейка: вместе не влезают.
	const half = 40000
	type attempt struct {
		outcome payment.ApplyOutcome
		err     error
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		attempts []attempt
	)
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			entry := refundEntry(in, capture, half, "refund:"+uuid.NewString(), now)
			res, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
				IntentID: in.ID, CaptureEntryID: capture.ID, Refund: entry, Now: now,
			})
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt{outcome: res.Outcome, err: err})
		}()
	}
	wg.Wait()

	outcomes := make([]payment.ApplyOutcome, 0, len(attempts))
	for _, a := range attempts {
		require.NoError(t, a.err)
		outcomes = append(outcomes, a.outcome)
	}

	assert.ElementsMatch(t,
		[]payment.ApplyOutcome{payment.OutcomeApplied, payment.OutcomeRefundTooLarge}, outcomes,
		"под блокировкой намерения второй возврат сверх зачисления отбит")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger WHERE kind = 'refund'`))

	// Второй рубеж: запись мимо сервиса отбивает триггер книги, и представляется
	// он ИМЕНЕМ ограничения — так же, как индекс.
	_, err := pool.Exec(t.Context(), `INSERT INTO payment_ledger (id, intent_id, kind, amount_minor,
		currency, provider_event_id, reverses_entry_id, idempotency_key, actor_id, created_at)
		VALUES ($1, $2, 'refund', $3, $4, '', $5, 'refund:bypass', NULL, $6)`,
		uuid.New(), in.ID, in.AmountMinor, in.Currency, capture.ID, now)
	assert.Equal(t, "payment_ledger_refund_cap", constraintOf(t, err),
		"нарушение представляется именем, а не одним кодом")

	// Возврат в чужой валюте — тоже отказ: сумма разных валют не значит ничего,
	// и потолок, посчитанный по ней, тоже.
	_, err = pool.Exec(t.Context(), `INSERT INTO payment_ledger (id, intent_id, kind, amount_minor,
		currency, provider_event_id, reverses_entry_id, idempotency_key, actor_id, created_at)
		VALUES ($1, $2, 'refund', 1, $3, '', $4, 'refund:ccy', NULL, $5)`,
		uuid.New(), in.ID, otherCurrency, capture.ID, now)
	assert.Equal(t, "payment_ledger_refund_currency", constraintOf(t, err))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger WHERE kind = 'refund'`))
}

// Тот же возврат, поданный дважды, не удваивает денег: идемпотентность строки
// книги — UNIQUE (intent_id, idempotency_key), а не ссылка на зачисление.
func TestStore_ApplyRefund_DuplicateKey(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	capture := settleIntent(t, store, in)
	now := testNow()

	entry := refundEntry(in, capture, 10000, "refund:same", now)
	first, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: entry, Now: now,
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, first.Outcome)

	retry := refundEntry(in, capture, 10000, "refund:same", now)
	second, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: retry, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeDuplicateEvent, second.Outcome)
	assert.Equal(t, entry.ID, second.Entry.ID, "вернулась уже записанная строка, а не новая")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger WHERE kind = 'refund'`))
}

// Форма запроса проверяется до всего остального: запись, уехавшая на другое
// намерение, прошла бы мимо блокировки, и потолок считался бы по чужой книге.
func TestStore_ApplyRefund_RejectsBadShape(t *testing.T) {
	t.Parallel()

	store, _, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	capture := settleIntent(t, store, in)
	now := testNow()

	tests := map[string]func(*payment.ApplyRefundRequest){
		"запись не возврат": func(req *payment.ApplyRefundRequest) {
			req.Refund.Kind = payment.LedgerCapture
		},
		"запись чужого намерения": func(req *payment.ApplyRefundRequest) {
			req.Refund.IntentID = uuid.New()
		},
		"гасит чужое зачисление": func(req *payment.ApplyRefundRequest) {
			other := uuid.New()
			req.Refund.ReversesEntryID = &other
		},
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := refundEntry(in, capture, 100, "refund:"+uuid.NewString(), now)
			req := payment.ApplyRefundRequest{
				IntentID: in.ID, CaptureEntryID: capture.ID, Refund: entry, Now: now,
			}
			mod(&req)
			_, err := store.ApplyRefund(t.Context(), req)
			require.ErrorIs(t, err, payment.ErrBadTransition)
		})
	}

	unknown := intent()
	entry := refundEntry(unknown, capture, 100, "refund:unknown", now)
	entry.ReversesEntryID = &capture.ID
	res, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: unknown.ID, CaptureEntryID: capture.ID, Refund: entry, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeUnknownIntent, res.Outcome)
}

// Книга append-only, и держит это база, а не дисциплина кода: правка в обход
// приложения — миграцией или руками в проде — обходит любой инвариант, который
// живёт только в Go.
func TestStore_LedgerIsAppendOnly(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	capture := settleIntent(t, store, in)

	tests := map[string]struct {
		sql  string
		args []any
	}{
		"UPDATE суммы":     {sql: `UPDATE payment_ledger SET amount_minor = 1 WHERE id = $1`, args: []any{capture.ID}},
		"UPDATE времени":   {sql: `UPDATE payment_ledger SET created_at = now() WHERE id = $1`, args: []any{capture.ID}},
		"DELETE записи":    {sql: `DELETE FROM payment_ledger WHERE id = $1`, args: []any{capture.ID}},
		"TRUNCATE таблицы": {sql: `TRUNCATE payment_ledger`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := pool.Exec(t.Context(), tt.sql, tt.args...)
			assert.Equal(t, "payment_ledger_immutable", constraintOf(t, err))
			assert.Contains(t, err.Error(), "append-only")
		})
	}

	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, capture.AmountMinor, entries[0].AmountMinor, "строка не изменилась")
	assert.Equal(t, capture.CreatedAt, entries[0].CreatedAt)
}

// constraintOf — имя ограничения, которым представилась база. Именно оно, а не
// текст: по имени адаптер и разбирает конфликт, а Error() имени не печатает.
func constraintOf(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	return pgErr.ConstraintName
}
