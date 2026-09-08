package paymentpg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
)

// quoted — строковые литералы SQL внутри окна ограничения.
var quoted = regexp.MustCompile(`'([^']*)'`)

// columnLine — строка объявления колонки в CREATE TABLE: четыре пробела, имя,
// тип с заглавной; CONSTRAINT, комментарии и переносы под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// Закрытые наборы ядра зеркалятся CHECK: значение, которое ядро знает, а база
// не принимает, — это отказ на записи денег; значение, которое принимает база,
// а ядро не знает, — ErrBadStatus на чтении уже записанной строки.
func TestChecksMirrorClosedSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		constraint string
		want       []string
	}{
		{"payment_intents_status_chk", asStrings(payment.AllStatuses)},
		{"payment_intents_confirmation_chk", append([]string{""}, asStrings(payment.AllConfirmationTypes)...)},
		{"payment_events_kind_chk", asStrings(payment.AllEventTypes)},
		{"payment_ledger_kind_chk", asStrings(payment.AllLedgerKinds)},
	}
	for _, tt := range tests {
		t.Run(tt.constraint, func(t *testing.T) {
			t.Parallel()
			body := constraintBody(t, tt.constraint)
			for _, value := range tt.want {
				assert.Contains(t, body, "'"+value+"'", "значение %q ядра нет в CHECK", value)
			}
			got := make([]string, 0, len(tt.want))
			for _, m := range quoted.FindAllStringSubmatch(body, -1) {
				got = append(got, m[1])
			}
			assert.ElementsMatch(t, tt.want, got, "CHECK и набор ядра разъехались")
		})
	}
}

// Выборки очереди сверки и частичный индекс под ними обязаны говорить об одних
// и тех же статусах: разъехавшись, они уводят из очереди целый статус, то есть
// прячут зависшие деньги.
func TestOpenStatuses_MatchSchemaFile(t *testing.T) {
	t.Parallel()

	for _, index := range []string{"ux_payment_intents_live_reference", "ix_payment_intents_open"} {
		body := after(t, "CREATE UNIQUE INDEX "+index, "CREATE INDEX "+index)
		got := make([]string, 0, len(openStatuses()))
		for _, m := range quoted.FindAllStringSubmatch(body, -1) {
			got = append(got, m[1])
		}
		assert.ElementsMatch(t, openStatuses(), got, "предикат индекса %s", index)
	}
}

// Ожидания CheckSchema живут в коде, схема — в файле: страж их расхождения.
func TestExpectedSchema_MatchesFile(t *testing.T) {
	t.Parallel()

	for table, spec := range expected {
		assert.Contains(t, Schema, "CREATE TABLE "+table+" (", table)
		for _, name := range spec.checks {
			assert.Contains(t, Schema, "CONSTRAINT "+name+" CHECK", name)
		}
		for name, unique := range spec.indexes {
			if strings.HasSuffix(name, "_pkey") {
				continue // имя первичного ключа Postgres даёт сам
			}
			assert.True(t,
				strings.Contains(Schema, "CONSTRAINT "+name+" UNIQUE") ||
					strings.Contains(Schema, "CONSTRAINT "+name+" PRIMARY KEY") ||
					strings.Contains(Schema, indexKeyword(unique)+name+" ON "+table),
				"индекса %s нет в schema.sql", name)
		}
		for _, name := range spec.triggers {
			assert.Contains(t, Schema, "CREATE TRIGGER "+name, name)
			assert.Contains(t, Schema, "ENABLE ALWAYS TRIGGER "+name, name)
		}
	}
	assertColumnsMatch(t)
}

// assertColumnsMatch — у каждой таблицы файла ровно те колонки, что ждёт
// CheckSchema: колонка, добавленная в схему и забытая в ожиданиях, не
// проверялась бы у потребителя ничем.
func assertColumnsMatch(t *testing.T) {
	t.Helper()

	for table, spec := range expected {
		declared := make([]string, 0, len(spec.columns))
		for _, m := range columnLine.FindAllStringSubmatch(tableBody(t, table), -1) {
			declared = append(declared, m[1])
		}
		wanted := make([]string, 0, len(spec.columns))
		for name := range spec.columns {
			wanted = append(wanted, name)
		}
		assert.ElementsMatch(t, wanted, declared, "колонки %s в schema.sql и в ожиданиях", table)
	}
}

// tableBody — текст объявления таблицы: от CREATE TABLE до закрывающей скобки.
// Тела функций-триггеров с их DECLARE сюда не попадают.
func tableBody(t *testing.T, table string) string {
	t.Helper()
	start := strings.Index(Schema, "CREATE TABLE "+table+" (")
	require.GreaterOrEqual(t, start, 0, "в schema.sql нет таблицы %s", table)
	end := strings.Index(Schema[start:], "\n);")
	require.GreaterOrEqual(t, end, 0, "объявление %s не закрыто", table)
	return Schema[start : start+end]
}

// Триггер потолка и триггер неизменяемости представляются ИМЕНАМИ ограничений:
// адаптер разбирает их так же, как нарушение индекса, — по имени, а не по коду.
func TestTriggerConstraintNames_AreInSchema(t *testing.T) {
	t.Parallel()

	for _, name := range []string{ckLedgerImmutable, ckLedgerRefundCap, ckLedgerRefundCurrency} {
		assert.Contains(t, Schema, "CONSTRAINT = '"+name+"'", name)
	}
}

func indexKeyword(unique bool) string {
	if unique {
		return "CREATE UNIQUE INDEX "
	}
	return "CREATE INDEX "
}

// constraintBody — текст объявления ограничения.
func constraintBody(t *testing.T, name string) string {
	t.Helper()
	return after(t, "CONSTRAINT "+name+" CHECK")
}

// after — объявление, начинающееся с одного из заголовков, до следующего
// объявления. Границу приходится искать явно: окно фиксированной длины
// захватило бы соседнее ограничение, и тест «CHECK ⊇ набора ядра» проходил бы
// за счёт чужих литералов.
func after(t *testing.T, headers ...string) string {
	t.Helper()
	for _, header := range headers {
		idx := strings.Index(Schema, header)
		if idx < 0 {
			continue
		}
		body := Schema[idx:]
		for _, stop := range []string{"\n    CONSTRAINT", "\n);", "\nCREATE", "\n--"} {
			if end := strings.Index(body, stop); end >= 0 {
				body = body[:end]
			}
		}
		return body
	}
	require.FailNowf(t, "в schema.sql нет объявления", "%v", headers)
	return ""
}

// asStrings — значения закрытого набора ядра как строки.
func asStrings[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}
