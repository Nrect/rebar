package authpg

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/token"
)

// columnLine — строка объявления колонки в CREATE TABLE: четыре пробела, имя,
// тип с заглавной; CONSTRAINT, комментарии и переносы CHECK под неё не подходят.
var columnLine = regexp.MustCompile(`(?m)^ {4}([a-z_]+)\s+[A-Z]`)

// Ожидания CheckSchema живут в коде, схема — в миграциях: страж их расхождения.
// Без него CheckSchema тихо перестаёт проверять колонку, которую в схему
// добавили, и потребитель узнаёт о расхождении не на старте, а на первом
// запросе.
func TestExpectedSchema_MatchesMigrations(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	blocks := tableBlocks(t)
	require.Len(t, blocks, len(expectedTables))

	for _, spec := range expectedTables {
		body, ok := blocks[spec.name]
		require.Truef(t, ok, "таблицы %s нет в миграциях", spec.name)

		matches := columnLine.FindAllStringSubmatch(body, -1)
		declared := make([]string, 0, len(matches))
		for _, m := range matches {
			declared = append(declared, m[1])
		}
		require.Lenf(t, declared, len(spec.columns),
			"%s: число колонок в миграциях и в expectedTables", spec.name)
		for _, name := range declared {
			assert.Containsf(t, spec.columns, name,
				"%s: колонка %s есть в миграциях, но не в expectedTables", spec.name, name)
		}
		for _, name := range spec.checks {
			assert.Contains(t, ddl, "CONSTRAINT "+name+" CHECK", name)
		}
		for name, unique := range spec.indexes {
			if strings.HasSuffix(name, "_pkey") {
				// Имя первичного ключа генерирует Postgres из имени таблицы;
				// в миграциях его нет, зато есть само объявление PRIMARY KEY.
				assert.Contains(t, body, "PRIMARY KEY", spec.name)
				continue
			}
			kind := "CREATE INDEX "
			if unique {
				kind = "CREATE UNIQUE INDEX "
			}
			assert.Contains(t, ddl, kind+name+" ON "+spec.name, name)
		}
	}
}

// CHECK ⊇ All*: закрытый набор кода и словарь базы обязаны совпадать, иначе
// расхождение всплывает в проде на первом новом значении.
func TestMigrations_PurposeCheckMirrorsAllPurposes(t *testing.T) {
	t.Parallel()

	ddl := schemaDDL(t)
	for _, purpose := range token.AllPurposes {
		assert.Containsf(t, ddl, "'"+purpose.String()+"'",
			"назначение %s объявлено в token.AllPurposes, но не в CHECK колонки purpose", purpose)
	}
	// И наоборот: словарь базы не шире набора кода — значение, которое база
	// принимает, а код не знает, нельзя ни погасить, ни применить.
	check, _, ok := strings.Cut(strings.SplitN(ddl, "purpose IN (", 2)[1], ")")
	require.True(t, ok)
	assert.Len(t, strings.Split(check, ","), len(token.AllPurposes))
}

// Форма реалма продублирована в CHECK каждой таблицы: реалм входит в ключи
// строк и в WHERE уборки, и разъезд кода с базой даёт строки, которые уже
// нельзя ни прочитать, ни удалить.
func TestMigrations_RealmCheckMirrorsRealmForm(t *testing.T) {
	t.Parallel()

	const form = "realm ~ '^[a-z0-9_]{1,32}$'"
	assert.Equal(t, 3, strings.Count(schemaDDL(t), form),
		"CHECK формы реалма обязан стоять во всех трёх таблицах")
	assert.Contains(t, form, "1,32", "потолок в схеме разошёлся с auth.MaxRealmLen")
	assert.Equal(t, 32, auth.MaxRealmLen)
}

// schemaDDL — каталог Migrations() текстом подряд, как его получит раннер
// потребителя. IF NOT EXISTS снят: здесь сверяются имена, а форму команд держат
// TestInitMigration_HoldsContract и повторный накат.
func schemaDDL(t *testing.T) string {
	t.Helper()
	migrations := Migrations()
	entries, err := fs.ReadDir(migrations, ".")
	require.NoError(t, err)
	var ddl strings.Builder
	for _, entry := range entries {
		raw, readErr := fs.ReadFile(migrations, entry.Name())
		require.NoError(t, readErr)
		ddl.Write(raw)
		ddl.WriteString("\n")
	}
	return strings.ReplaceAll(ddl.String(), " IF NOT EXISTS ", " ")
}

// Адаптер читает и пишет ровно те колонки, которые проверяет CheckSchema.
func TestSessionColumns_AreAllExpected(t *testing.T) {
	t.Parallel()

	spec := specOf(t, tableSessions)
	for _, raw := range strings.Split(sessionColumns, ",") {
		name := strings.TrimSpace(raw)
		assert.Containsf(t, spec.columns, name, "колонка %s читается адаптером, но не проверяется", name)
	}
}

// tableBlocks — тело каждого CREATE TABLE из миграций.
func tableBlocks(t *testing.T) map[string]string {
	t.Helper()

	blocks := map[string]string{}
	for _, chunk := range strings.Split(schemaDDL(t), "CREATE TABLE ")[1:] {
		name, body, ok := strings.Cut(chunk, " (")
		require.True(t, ok)
		body, _, ok = strings.Cut(body, "\n);")
		require.True(t, ok)
		blocks[strings.TrimSpace(name)] = body
	}
	return blocks
}

func specOf(t *testing.T, name string) tableSpec {
	t.Helper()
	for _, spec := range expectedTables {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("таблицы %s нет в expectedTables", name)
	return tableSpec{}
}
