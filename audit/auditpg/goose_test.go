package auditpg_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
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
// проверяются в файле, а не в его копии. Заодно это проверка допущения, на
// котором стоит gooseDown: маркеров ровно по одному и Up идёт раньше Down.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw := readSchema(t)
	up := pgtest.GooseUp(t, schemaPath)
	down := gooseDown(t)

	require.Equal(t, 1, strings.Count(raw, pgtest.GooseUpMarker), "маркер Up обязан быть один")
	require.Equal(t, 1, strings.Count(raw, pgtest.GooseDownMarker), "маркер Down обязан быть один")
	require.Less(t, strings.Index(raw, pgtest.GooseUpMarker), strings.Index(raw, pgtest.GooseDownMarker),
		"Down обязана идти после Up: на этом стоит разбор обратной секции")

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

	assert.Equal(t, readSchema(t), auditpg.Schema)
}

// CHECK ⊇ All*: словарь кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestSchemaFile_ChecksMirrorClosedSets(t *testing.T) {
	t.Parallel()

	up := pgtest.GooseUp(t, schemaPath)

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
