package paymentpg_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres/pgtest"
)

// schemaPath — файл схемы; он же артефакт для goose потребителя.
const schemaPath = "schema.sql"

// readSchema — файл целиком.
func readSchema(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	require.NoError(t, err)
	return string(raw)
}

// gooseDown — тело обратной секции. Секцию Up отдаёт pgtest.GooseUp; вторая
// копия разбора завела бы вторую правду о строке директивы, а в схемах тулкита
// Down последняя, поэтому это всё, что идёт после её маркера. Допущение
// проверяет TestSchemaFile_HoldsContract: без него переставленные секции
// отдали бы обрезанное тело вместо падения.
func gooseDown(t *testing.T) string {
	t.Helper()
	_, down, ok := strings.Cut(readSchema(t), pgtest.GooseDownMarker)
	require.True(t, ok, "в %s нет маркера %q", schemaPath, pgtest.GooseDownMarker)
	return down
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии в коде. Заодно это проверка допущения,
// на котором стоит gooseDown: маркеров ровно по одному и Up идёт раньше Down.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw := readSchema(t)
	up := pgtest.GooseUp(t, schemaPath)
	down := gooseDown(t)

	require.Equal(t, 1, strings.Count(raw, pgtest.GooseUpMarker), "маркер Up обязан быть один")
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseDownMarker), "маркер Down обязан быть один")
	require.Less(t, strings.Index(raw, pgtest.GooseUpMarker), strings.Index(raw, pgtest.GooseDownMarker),
		"Down обязана идти после Up: на этом стоит разбор обратной секции")

	// Тела функций с ';' внутри обёрнуты в StatementBegin/End для раннера
	// потребителя; разбор pgtest их не считает границей секции, иначе схема
	// применялась бы без триггеров книги, а тесты зеленели бы на ней впустую.
	assert.Contains(t, up, "RETURN NEW", "тело функции не обрывается на StatementBegin")
	assert.NotContains(t, up, "+goose", "директивы раннера в тело не попадают")

	for _, want := range []string{
		"CREATE TABLE payment_intents",
		"CREATE TABLE payment_intent_items",
		"CREATE TABLE payment_events",
		"CREATE TABLE payment_ledger",
		// Имена, по которым адаптер разбирает конфликт.
		"ux_payment_intents_key",
		"ux_payment_intents_live_reference",
		"ux_payment_events_dedup",
		"ux_payment_ledger_capture",
		"ux_payment_ledger_key",
		// Книгу держит база, а не код.
		"ENABLE ALWAYS TRIGGER payment_ledger_immutable_trg",
		"ENABLE ALWAYS TRIGGER payment_ledger_no_truncate_trg",
		"ENABLE ALWAYS TRIGGER payment_ledger_refund_cap_trg",
	} {
		assert.Contains(t, up, want)
	}
	// Время только параметром: DEFAULT now() в доменной колонке — вторая правда
	// о времени, и тест на управляемых часах проверял бы не то, что пишет база.
	assert.NotContains(t, strings.ToLower(up), "default now()")
	assert.NotContains(t, strings.ToLower(up), "current_timestamp")
	// Имени схемы в таблицах нет: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.")
	assert.NotContains(t, up, "DROP TABLE")

	for _, want := range []string{
		"DROP TABLE payment_ledger",
		"DROP TABLE payment_events",
		"DROP TABLE payment_intent_items",
		"DROP TABLE payment_intents",
		"DROP FUNCTION payment_ledger_refund_cap",
		"DROP FUNCTION payment_ledger_immutable",
	} {
		assert.Contains(t, down, want)
	}
	assert.NotContains(t, down, "CREATE TABLE")
}
