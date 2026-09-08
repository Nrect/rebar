package paymentpg_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// gooseSection — тело секции goose: строки между маркером секции и следующим
// маркером секции либо концом файла.
//
// Своя, а не pgtest.GooseUp: у схемы есть тела функций с ';' внутри, и они
// обязаны быть обёрнуты в StatementBegin/End для раннера потребителя. pgtest
// считает секцией ЛЮБУЮ директиву «-- +goose», поэтому на StatementBegin он
// обрывает секцию и молча отдаёт обрезанную схему.
func gooseSection(sql, marker string) (body string, ok bool) {
	var out strings.Builder
	inside := false
	for line := range strings.SplitSeq(sql, "\n") {
		directive := strings.TrimSpace(line)
		if directive == gooseUp || directive == gooseDown {
			inside = directive == marker
			ok = ok || inside
			continue
		}
		if strings.HasPrefix(directive, "-- +goose") {
			continue // StatementBegin/End — разметка раннера, а не текст SQL
		}
		if inside {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String(), ok
}

func TestGooseSection(t *testing.T) {
	t.Parallel()

	const doc = "-- заголовок\n" +
		gooseUp + "\nCREATE TABLE t (id INT);\n" +
		"-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS trigger AS $$\nBEGIN\nRETURN NEW;\nEND;\n$$;\n" +
		"-- +goose StatementEnd\nCREATE TRIGGER g BEFORE INSERT ON t EXECUTE FUNCTION f();\n" +
		gooseDown + "\nDROP TABLE t;\n"

	up, ok := gooseSection(doc, gooseUp)
	require.True(t, ok)
	assert.Contains(t, up, "CREATE TABLE t")
	assert.Contains(t, up, "RETURN NEW;", "тело функции не обрывается на StatementBegin")
	assert.Contains(t, up, "CREATE TRIGGER g", "секция продолжается после StatementEnd")
	assert.NotContains(t, up, "+goose", "маркеры в SQL не уезжают")
	assert.NotContains(t, up, "DROP TABLE")
	assert.NotContains(t, up, "-- заголовок", "текст до первого маркера не в секции")

	down, ok := gooseSection(doc, gooseDown)
	require.True(t, ok)
	assert.Contains(t, down, "DROP TABLE t")
	assert.NotContains(t, down, "CREATE TABLE")

	_, ok = gooseSection(doc, "-- +goose Nope")
	assert.False(t, ok, "маркера нет — секции нет")
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии в коде.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok)
	down, ok := gooseSection(raw, gooseDown)
	require.True(t, ok)

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
