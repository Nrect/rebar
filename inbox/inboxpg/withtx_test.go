package inboxpg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
	"github.com/nrect/rebar/postgres"
)

// abortMessage — узнаваемый текст прерывания (уточнение арбитра 1).
const abortMessage = "inboxpg: транзакция прервана после отказа"

// insertOrder — бизнес-факт потребителя в его транзакции.
func insertOrder(t *testing.T, tx pgx.Tx, id string) {
	t.Helper()
	_, err := tx.Exec(t.Context(), `INSERT INTO shop_orders (id) VALUES ($1)`, id)
	require.NoError(t, err, "факт потребителя %s", id)
}

// WithTx — транзакция потребителя: отметка, тело и эффект обработчика ложатся
// вместе с его бизнес-фактом и вместе откатываются, а ключ после отката свободен.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(handlerFunc(recordEffect)))
	shopTables(t, pool)
	ev := testEvent("evt-withtx", "withtx")

	tx := beginTx(t, pool)
	insertOrder(t, tx, "order-1")
	mustAccept(t, store.WithTx(tx), ev, inbox.OutcomeAccepted)
	require.NoError(t, tx.Rollback(t.Context()))
	assert.Equal(t, [3]int{}, stored(t, pool, ev.ID), "отметка, тело или эффект пережили откат")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_orders`))

	tx = beginTx(t, pool)
	insertOrder(t, tx, "order-1")
	mustAccept(t, store.WithTx(tx), ev, inbox.OutcomeAccepted)
	require.NoError(t, tx.Commit(t.Context()))
	assert.Equal(t, [3]int{1, 1, 1}, stored(t, pool, ev.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
}

// errPanicked — паника обработчика, перехваченная тестом.
var errPanicked = errors.New("inboxpg_test: handler panicked")

// Уточнение арбитра 1: после любой ошибки Accept и Purge в режиме WithTx COMMIT
// потребителя не проходит, и не остаются ни его факт, ни отметка, ни эффект.
//
// COMMIT ПРЕРВАННОЙ ТРАНЗАКЦИИ POSTGRES ПРИНИМАЕТ КАК ROLLBACK, БЕЗ ОШИБКИ: pgx
// отдаёт pgx.ErrTxCommitRollback с общим текстом. Причину поэтому видно в
// последней ошибке соединения — так её покажет трассировщик потребителя: отказ
// адаптера с узнаваемым текстом и кодом, который postgres.IsRetryable не
// повторяет, либо ошибка базы, прервавшая транзакцию раньше.
func TestStore_WithTx_AbortsOnError(t *testing.T) {
	t.Parallel()

	errRefused := errors.New("inboxpg_test: handler refused")
	store, pool, traced := newTracedStore(t, only(handlerFunc(func(ctx context.Context, tx pgx.Tx, ev inbox.Event) error {
		if err := recordEffect(ctx, tx, ev); err != nil {
			return err
		}
		switch ev.ID {
		case "evt-abort-refused":
			return errRefused
		case "evt-abort-swallowed":
			_, _ = tx.Exec(ctx, `SELECT 1/0`)
		case "evt-abort-panic":
			panic(errPanicked)
		}
		return nil
	})))
	shopTables(t, pool)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	accept := func(ctx context.Context, ev inbox.Event) func(*inboxpg.Store) error {
		return func(s *inboxpg.Store) (err error) {
			defer func() {
				if recover() != nil {
					err = errPanicked
				}
			}()
			_, err = s.Accept(ctx, ev, testNow)
			return err
		}
	}
	purge := func(ctx context.Context, limit int) func(*inboxpg.Store) error {
		return func(s *inboxpg.Store) error {
			_, err := s.Purge(ctx, testNow, testNow, limit)
			return err
		}
	}
	ours := func(t *testing.T, cause *pgconn.PgError) {
		t.Helper()
		assert.Equal(t, "25P02", cause.Code, "код прерывания")
		assert.Equal(t, abortMessage, cause.Message, "текст прерывания")
		assert.False(t, postgres.IsRetryable(cause), "прерывание не повторяют")
	}
	byDatabase := func(code, constraint string) func(*testing.T, *pgconn.PgError) {
		return func(t *testing.T, cause *pgconn.PgError) {
			t.Helper()
			assert.Equal(t, code, cause.Code, "транзакцию прервала база")
			assert.Equal(t, constraint, cause.ConstraintName)
		}
	}
	unhandled := testEvent("evt-abort-unhandled", "x")
	unhandled.Source = "absent"
	invalid := testEvent("", "x")

	for _, tc := range []struct {
		name  string
		fail  func(*inboxpg.Store) error
		cause func(*testing.T, *pgconn.PgError)
	}{
		{"ошибка обработчика", accept(t.Context(), testEvent("evt-abort-refused", "x")), ours},
		{"паника обработчика", accept(t.Context(), testEvent("evt-abort-panic", "x")), ours},
		{"источник без обработчика", accept(t.Context(), unhandled), ours},
		{"отменённый контекст", accept(cancelled, testEvent("evt-abort-cancelled", "x")), ours},
		{"уборка с непозитивным потолком", purge(t.Context(), 0), ours},
		{"уборка по отменённому контексту", purge(cancelled, 10), ours},
		{"обработчик проглотил ошибку базы", accept(t.Context(), testEvent("evt-abort-swallowed", "x")), byDatabase("22012", "")},
		{"схема отвергла событие", accept(t.Context(), invalid), byDatabase("23514", "inbox_events_id_chk")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t, pool)
			traced.forget(tx.Conn())
			insertOrder(t, tx, tc.name)

			require.Error(t, tc.fail(store.WithTx(tx)))
			// Причина читается до COMMIT: после него соединение уходит в пул к соседу.
			var cause *pgconn.PgError
			require.ErrorAs(t, traced.lastOn(tx.Conn()), &cause, "причины прерывания в транзакции нет")
			tc.cause(t, cause)

			require.ErrorIs(t, tx.Commit(t.Context()), pgx.ErrTxCommitRollback, "COMMIT после отказа прошёл")
			assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_orders WHERE id = $1`, tc.name), "факт потребителя закоммичен")
		})
	}
	t.Cleanup(func() {
		assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM inbox_events`), "отметка закоммичена")
		assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_effects`), "эффект закоммичен")
	})
}

// duplicate, conflict и in_flight — исходы, а не ошибки: транзакция потребителя
// после них живёт и фиксируется. Перехват 23505 вместо ON CONFLICT прервал бы
// её на первом же повторе.
func TestStore_WithTx_RepeatKeepsTxUsable(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	shopTables(t, pool)
	ev := testEvent("evt-repeat", "repeat")
	mustAccept(t, store, ev, inbox.OutcomeAccepted)

	// Ключ второго события держит чужая транзакция до конца теста.
	held := testEvent("evt-held", "held")
	holder := beginTx(t, pool)
	mustAccept(t, store.WithTx(holder), held, inbox.OutcomeAccepted)

	tx := beginTx(t, pool)
	inTx := store.WithTx(tx)
	for _, tc := range []struct {
		name string
		ev   inbox.Event
		want inbox.Outcome
	}{
		{"повтор", ev, inbox.OutcomeDuplicate},
		{"другой отпечаток", testEvent(ev.ID, "other body"), inbox.OutcomeConflict},
		{"ключ в чужой транзакции", held, inbox.OutcomeInFlight},
	} {
		mustAccept(t, inTx, tc.ev, tc.want)
		insertOrder(t, tx, tc.name)
	}
	require.NoError(t, tx.Commit(t.Context()), "транзакция потребителя после повторов")
	assert.Equal(t, 3, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
	require.NoError(t, holder.Rollback(t.Context()))
}
