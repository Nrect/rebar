package paymentpg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment/paymentpg"
)

// CheckSchema сверяет, но не применяет: на пустой схеме он обязан назвать
// таблицы и сказать, что делать, а не молча создать их.
func TestCheckSchema_MissingTables(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	store := paymentpg.New(pool, paymentpg.Options{})

	err := store.CheckSchema(t.Context())
	require.Error(t, err)
	for _, table := range []string{"payment_intents", "payment_intent_items", "payment_events", "payment_ledger"} {
		assert.Contains(t, err.Error(), table)
	}
	assert.Contains(t, err.Error(), "накатите paymentpg.Migrations() раннером проекта",
		"первая строка говорит, что делать")

	var tables int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tables))
	assert.Zero(t, tables, "CheckSchema ничего не применяет")
}

// Расхождения перечисляются все сразу, а колонка потребителя расхождением не
// считается: свои колонки он вправе добавлять.
func TestCheckSchema_ReportsMismatches(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	_, err := pool.Exec(t.Context(), `ALTER TABLE payment_intents ADD COLUMN shop_id UUID`)
	require.NoError(t, err)
	require.NoError(t, store.CheckSchema(t.Context()), "лишняя колонка потребителя — не расхождение")

	_, err = pool.Exec(t.Context(), `DROP INDEX ix_payment_intents_open`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `ALTER TABLE payment_ledger DROP CONSTRAINT payment_ledger_refund_chk`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `ALTER TABLE payment_events DROP COLUMN deliveries`)
	require.NoError(t, err)

	err = store.CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "накатите paymentpg.Migrations() раннером проекта",
		"первая строка говорит, что делать")
	assert.Contains(t, err.Error(), "ix_payment_intents_open")
	assert.Contains(t, err.Error(), "payment_ledger_refund_chk")
	assert.Contains(t, err.Error(), "deliveries")
}

// Триггер в режиме по умолчанию молчит при репликации — то есть ровно там, где
// книгу правят в обход приложения. CheckSchema обязан это заметить.
func TestCheckSchema_TriggerNotAlways(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, paymentpg.Options{})
	_, err := pool.Exec(t.Context(),
		`ALTER TABLE payment_ledger ENABLE REPLICA TRIGGER payment_ledger_immutable_trg`)
	require.NoError(t, err)

	err = store.CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "payment_ledger_immutable_trg")
	assert.Contains(t, err.Error(), "ENABLE ALWAYS")
}
