package auditpg_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
)

const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// gooseSection — тело секции goose: строки между маркером и следующим
// маркером секции или концом файла.
//
// StatementBegin/End пропускаются, а не переключают секцию: они нужны раннеру
// goose, чтобы не резать тело функции по ';', а pgx выполняет тело секции
// одним простым запросом и сам их не понимает. Приняв их за границу секции,
// разбор потерял бы триггер append-only и тест зеленел бы на схеме без него.
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
			continue
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
		"-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS INT AS $$ BEGIN RETURN 1; END; $$;\n" +
		"-- +goose StatementEnd\n" +
		gooseDown + "\nDROP TABLE t;\n"

	tests := []struct {
		name    string
		marker  string
		wantOK  bool
		want    string
		notWant string
	}{
		{name: "Up без Down", marker: gooseUp, wantOK: true, want: "CREATE TABLE t", notWant: "DROP TABLE"},
		{name: "Down без Up", marker: gooseDown, wantOK: true, want: "DROP TABLE t", notWant: "CREATE TABLE"},
		{name: "маркера нет", marker: "-- +goose Nope", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, ok := gooseSection(doc, tt.marker)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				assert.Empty(t, body)
				return
			}
			assert.Contains(t, body, tt.want)
			assert.NotContains(t, body, tt.notWant, "секции не перетекают друг в друга")
			assert.NotContains(t, body, "-- заголовок", "текст до первого маркера не в секции")
		})
	}

	up, ok := gooseSection(doc, gooseUp)
	require.True(t, ok)
	assert.Contains(t, up, "CREATE FUNCTION f()", "StatementBegin не обрывает секцию")
	assert.NotContains(t, up, "+goose Statement", "директивы раннера в тело не попадают")
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok)
	down, ok := gooseSection(raw, gooseDown)
	require.True(t, ok)

	for _, want := range []string{
		"CREATE TABLE audit_events",
		"ix_audit_events_occurred_at",
		"ix_audit_events_actor",
		"ix_audit_events_target",
		"audit_events_outcome_chk",
		"audit_events_actor_kind_chk",
		"audit_events_append_only_trg",
		"ENABLE ALWAYS TRIGGER",
	} {
		assert.Contains(t, up, want)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.Contains(t, down, "DROP TABLE audit_events")
	assert.NotContains(t, down, "CREATE TABLE")

	// Время приходит параметром: DEFAULT now() дал бы вторую правду о времени.
	assert.NotContains(t, up, "now()")
	// Без имени схемы: search_path выбирает потребитель.
	assert.NotContains(t, up, "public.audit_events")
	// Без FK на таблицы потребителя: их имён пакет не знает.
	assert.NotContains(t, up, "REFERENCES")
}

// Schema должен быть побайтно равен файлу: потребитель применяет либо файл,
// либо строку, и разъезд между ними виден только в проде.
func TestSchemaConst_MatchesFile(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	assert.Equal(t, raw, auditpg.Schema)
}

// CHECK ⊇ All*: словарь кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestSchemaFile_ChecksMirrorClosedSets(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok)

	outcomes := checkValues(t, up, "audit_events_outcome_chk")
	for _, o := range audit.AllOutcomes {
		assert.Contains(t, outcomes, string(o), "исход %q не зеркалится CHECK", o)
	}
	assert.Len(t, outcomes, len(audit.AllOutcomes), "CHECK и AllOutcomes разъехались")

	kinds := checkValues(t, up, "audit_events_actor_kind_chk")
	for _, k := range audit.AllActorKinds {
		assert.Contains(t, kinds, string(k), "род актора %q не зеркалится CHECK", k)
	}
	assert.Len(t, kinds, len(audit.AllActorKinds), "CHECK и AllActorKinds разъехались")

	// У action CHECK'а нет намеренно: набор задаёт Config.Actions потребителя.
	assert.NotContains(t, up, "audit_events_action_chk")
}

// checkValues — литералы из IN (...) именованного CHECK.
func checkValues(t *testing.T, sql, constraint string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(sql, "CONSTRAINT "+constraint)
	require.True(t, ok, "в схеме нет ограничения %s", constraint)
	_, rest, ok = strings.Cut(rest, "IN (")
	require.True(t, ok, "у %s нет списка IN (...)", constraint)
	list, _, ok := strings.Cut(rest, ")")
	require.True(t, ok)

	items := strings.Split(list, ",")
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, strings.Trim(strings.TrimSpace(item), "'"))
	}
	return out
}
