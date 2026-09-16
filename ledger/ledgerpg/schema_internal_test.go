package ledgerpg

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// migrationUp — секции Up каталога Migrations() текстом подряд, как их получит
// раннер потребителя.
func migrationUp(t *testing.T) string {
	t.Helper()
	migrations := Migrations()
	entries, err := fs.ReadDir(migrations, ".")
	require.NoError(t, err)
	var up strings.Builder
	for _, entry := range entries {
		raw, readErr := fs.ReadFile(migrations, entry.Name())
		require.NoError(t, readErr)
		section, _, ok := strings.Cut(string(raw), "-- +goose Down")
		require.True(t, ok, "в %s нет секции Down", entry.Name())
		up.WriteString(section)
		up.WriteString("\n")
	}
	return up.String()
}

// columnLine — объявление колонки в CREATE TABLE: четыре пробела, имя, тип
// заглавными; CONSTRAINT и комментарии под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// tableBody — объявление таблицы от CREATE TABLE до закрывающей скобки.
func tableBody(t *testing.T, up, table string) string {
	t.Helper()
	start := strings.Index(up, "CREATE TABLE IF NOT EXISTS "+table+" (")
	require.GreaterOrEqual(t, start, 0, "в миграции нет таблицы %s", table)
	end := strings.Index(up[start:], "\n);")
	require.GreaterOrEqual(t, end, 0, "объявление %s не закрыто", table)
	return up[start : start+end]
}

// Ожидания CheckSchema живут в коде, схема — в миграциях: страж их расхождения.
// Колонка, CHECK или триггер, добавленные в файл и забытые здесь, не
// проверялись бы у потребителя ничем.
func TestExpectedSchema_MatchesMigrations(t *testing.T) {
	t.Parallel()

	up := migrationUp(t)
	for _, table := range slices.Sorted(maps.Keys(expected)) {
		spec := expected[table]
		body := tableBody(t, up, table)

		declared := make([]string, 0, len(spec.columns))
		for _, m := range columnLine.FindAllStringSubmatch(body, -1) {
			declared = append(declared, m[1])
		}
		assert.ElementsMatch(t, slices.Collect(maps.Keys(spec.columns)), declared, "колонки %s", table)

		constraints := regexp.MustCompile(`CONSTRAINT (\w+) (CHECK|FOREIGN KEY)`).FindAllStringSubmatch(body, -1)
		names := make([]string, 0, len(constraints))
		for _, m := range constraints {
			names = append(names, m[1])
		}
		assert.ElementsMatch(t, spec.constraints, names, "CHECK и внешние ключи %s", table)

		for name := range spec.indexes {
			assert.True(t, strings.Contains(body, "CONSTRAINT "+name+" PRIMARY KEY") ||
				strings.Contains(body, "CONSTRAINT "+name+" UNIQUE") ||
				strings.Contains(up, "CREATE UNIQUE INDEX IF NOT EXISTS "+name+" ON "+table+" "),
				"индекса %s нет в миграции", name)
		}
		for name, trigger := range spec.triggers {
			assertTriggerDeclared(t, up, table, name, trigger)
		}
	}
}

func assertTriggerDeclared(t *testing.T, up, table, name string, want triggerSpec) {
	t.Helper()
	declared := regexp.MustCompile(`DROP TRIGGER IF EXISTS ` + name + ` ON ` + table + `;\nCREATE TRIGGER ` + name +
		`\n\s+(BEFORE|AFTER) [A-Z ]+ ON ` + table +
		`\n\s+FOR EACH (ROW|STATEMENT) EXECUTE FUNCTION ` + want.function + `\(\);\nALTER TABLE ` + table +
		` ENABLE ALWAYS TRIGGER ` + name + `;`)
	assert.Regexp(t, declared, up, "триггер %s: DROP, CREATE со своей функцией и ENABLE ALWAYS подряд", name)

	header := regexp.MustCompile(`CREATE OR REPLACE FUNCTION ` + want.function + `\(\) RETURNS trigger\nLANGUAGE plpgsql( SECURITY DEFINER)? AS`)
	m := header.FindStringSubmatch(up)
	require.NotNil(t, m, "функции %s нет в миграции", want.function)
	assert.Equal(t, want.definer, m[1] != "", "SECURITY DEFINER у функции %s", want.function)

	pinned := regexp.MustCompile(`ARRAY\[([^\]]+)\] LOOP`).FindStringSubmatch(up)
	require.NotNil(t, pinned, "в миграции нет закрепления search_path")
	assert.Contains(t, pinned[1], "'"+want.function+"'", "search_path функции %s не закрепляется", want.function)
	assert.Greater(t, strings.Index(up, "FOREACH fn IN ARRAY"), strings.LastIndex(up, "CREATE OR REPLACE FUNCTION"),
		"CREATE OR REPLACE FUNCTION сбрасывает SET: закрепление идёт после всех функций")
}

// raisedByTrigger — имена, которыми представляются триггеры, и их SQLSTATE.
var raisedByTrigger = regexp.MustCompile(`ERRCODE = '(\w{5})', CONSTRAINT = '(\w+)'`)

// notFromAdapter — отказы, которых адаптер не вызывает: он не правит ни журнал,
// ни голову счёта.
var notFromAdapter = []string{fnEntriesImmutable, fnAccountsGuard}

// Каждое имя, которым база отказывает записи, разобрано в refusals, и класс
// разбора совпадает с SQLSTATE: 40001 — сбой, который повторяют, остальное —
// отказ, который не повторяют никогда (решение 2). Обратно: имя в refusals
// объявлено в миграции — опечатка иначе молча превращала бы отказ в сбой.
func TestRefusals_CoverEveryName(t *testing.T) {
	t.Parallel()

	up := migrationUp(t)
	raised := map[string]string{}
	for _, m := range raisedByTrigger.FindAllStringSubmatch(up, -1) {
		if code, seen := raised[m[2]]; seen {
			assert.Equal(t, code, m[1], "%s поднимается разными SQLSTATE", m[2])
		}
		raised[m[2]] = m[1]
	}
	for _, name := range slices.Sorted(maps.Keys(raised)) {
		code := raised[name]
		if slices.Contains(notFromAdapter, name) {
			assert.NotContains(t, refusals, name)
			continue
		}
		sentinel, ok := refusals[name]
		if !assert.True(t, ok, "имя %s не разобрано в refusals", name) {
			continue
		}
		assert.Equal(t, code == "40001", errors.Is(sentinel, ledger.ErrUnavailable), "%s (%s): класс разбора", name, code)
	}
	for name := range refusals {
		_, isRaised := raised[name]
		assert.True(t, isRaised || strings.Contains(up, "CONSTRAINT "+name+" "), "имени %s нет в миграции", name)
	}
}

// Закрытые наборы и потолки ядра зеркалятся схемой: значение, которое ядро
// знает, а база не примет, — отказ на записи денег.
func TestChecksMirrorCore(t *testing.T) {
	t.Parallel()

	up := migrationUp(t)
	inList := func(constraint string) []string {
		m := regexp.MustCompile(`CONSTRAINT ` + constraint + ` CHECK \(\w+ IN \(([^)]*)\)\)`).FindStringSubmatch(up)
		require.NotNil(t, m, "нет CHECK %s", constraint)
		items := strings.Split(m[1], ",")
		values := make([]string, 0, len(items))
		for _, v := range items {
			values = append(values, strings.Trim(strings.TrimSpace(v), "'"))
		}
		return values
	}
	assert.ElementsMatch(t, strs(ledger.AllSigns), inList("ledger_kinds_sign_chk"))
	assert.ElementsMatch(t, strs(ledger.AllRequirements), inList("ledger_kinds_reference_chk"))
	assert.ElementsMatch(t, strs(ledger.AllRequirements), inList("ledger_kinds_attribution_chk"))

	capacity := strconv.FormatInt(ledger.MaxAmountMinor, 10)
	assert.Equal(t, 2, strings.Count(up, "BETWEEN -"+capacity+" AND "+capacity), "потолок суммы в CHECK и в триггере")
	size := strconv.Itoa(ledger.HashSize)
	assert.Contains(t, up, "octet_length(prev_hash) = "+size+" AND octet_length(entry_hash) = "+size, "длина подписи в CHECK")
	assert.Contains(t, up, "octet_length(NEW.prev_hash) <> "+size+" OR octet_length(NEW.entry_hash) <> "+size, "и в триггере")
	assert.Contains(t, up, "{1,"+strconv.Itoa(ledger.MaxNameLen)+"}$'", "имя книги и рода")
	assert.Contains(t, up, "{1,"+strconv.Itoa(ledger.MaxUnitLen)+"}$'", "единица книги")
	assert.Contains(t, up, "kind <> '"+ledger.KindReversal+"'", "род отмены")
}

func strs[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}

func TestPinsSearchPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		config []string
		want   bool
	}{
		{[]string{"search_path=ledger, pg_temp"}, true},
		{[]string{"work_mem=1MB", `search_path="My Schema", pg_temp`}, true},
		{[]string{"search_path=pg_temp, ledger"}, false},
		{[]string{"search_path=ledger"}, false},
		{[]string{"work_mem=1MB"}, false},
		{nil, false},
	} {
		assert.Equal(t, tc.want, pinsSearchPath(tc.config), "%q", tc.config)
	}
}

// Граница разбора: отказ — своей sentinel поверх очищенной ошибки, *PgError с
// Detail наружу не уходит ни в одной ветке.
func TestRefusal_Boundary(t *testing.T) {
	t.Parallel()

	raw := &pgconn.PgError{Code: "23514", ConstraintName: ckEntriesFloor, Message: "below floor", Detail: "Failing row contains (secret)"}
	err := refusal("insert entry", raw)
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	var clean *postgres.Error
	require.ErrorAs(t, err, &clean)
	assert.Equal(t, ckEntriesFloor, clean.Constraint)
	var pgErr *pgconn.PgError
	assert.NotErrorAs(t, err, &pgErr)
	assert.NotContains(t, err.Error(), "secret")

	unknown := refusal("insert entry", &pgconn.PgError{Code: "23514", ConstraintName: "consumer_chk", Detail: "secret"})
	require.ErrorIs(t, unknown, ledger.ErrUnavailable, "чужое ограничение — сбой, а не отказ домена")
	assert.NotErrorAs(t, unknown, &pgErr)

	cancelled := refusal("insert entry", context.Canceled)
	require.ErrorIs(t, cancelled, ledger.ErrUnavailable)
	require.ErrorIs(t, cancelled, context.Canceled)

	assert.NoError(t, refusal("insert entry", nil))
}
