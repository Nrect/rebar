package paymentpg_test

// Копии помощников pgtest из main (ApplyUp, ApplyDown, CheckMigrations,
// SchemaObjects): модуль пинит postgres v0.2.0, где их ещё нет.
// TODO(ADR-0011): после тега postgres файл удалить, вызовы — на pgtest.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// migrationsDir — каталог миграций на диске: pgtest v0.2.0 читает секцию по
// пути, а не из fs.FS. Что embed отдаёт тот же каталог, держит
// TestMigrations_Catalog.
const migrationsDir = "migrations"

// migrationName — номер в пять знаков, как у `goose create -s` (ADR-0011,
// решение 1).
var migrationName = regexp.MustCompile(`^(\d{5})_.+\.sql$`)

// applyUp — секции Up каталога по возрастанию номера, файл одним запросом.
//
// TODO(ADR-0011): pgtest.ApplyUp после тега postgres.
func applyUp(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, name := range catalog(t) {
		pgtest.Apply(t, pool, pgtest.GooseUp(t, filepath.Join(migrationsDir, name)))
	}
}

// applyDown — секции Down по убыванию номера. Цикл по срезу, а не по
// slices.Backward: тело range-over-func — замыкание без t.Helper, и падение
// указывало бы сюда, а не на строку теста.
//
// TODO(ADR-0011): pgtest.ApplyDown после тега postgres.
func applyDown(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	names := catalog(t)
	slices.Reverse(names)
	for _, name := range names {
		pgtest.Apply(t, pool, gooseDown(t, name))
	}
}

// catalog — файлы Migrations() по возрастанию номера. Каталог с находками
// стражей не накатывается: тест применил бы не ту последовательность, что
// раннер потребителя.
func catalog(t *testing.T) []string {
	t.Helper()
	names, findings := catalogFindings(paymentpg.Migrations())
	require.Empty(t, findings, "каталог миграций не применяется")
	return names
}

// gooseDown — тело секции Down: всё после её маркера. Разбора Down в pgtest
// v0.2.0 нет; что Down в файле одна и последняя, держат стражи каталога.
func gooseDown(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(migrationsDir, name))
	require.NoError(t, err)
	lines := strings.Split(string(raw), "\n")
	at := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) == pgtest.GooseDownMarker
	})
	require.GreaterOrEqual(t, at, 0, "в %s нет маркера %q", name, pgtest.GooseDownMarker)
	return strings.Join(lines[at+1:], "\n")
}

// catalogFindings — стражи каталога (ADR-0011, решение 7, пункты 1–3): номера
// подряд с 00001 без повторов, файлов без номера нет, у каждого файла по одной
// секции Up и Down, и Up раньше. ReadDir отдаёт записи по имени, а номер
// фиксированной ширины делает порядок имён порядком номеров.
//
// TODO(ADR-0011): pgtest.CheckMigrations после тега postgres.
func catalogFindings(fsys fs.FS) (names, findings []string) {
	entries, err := fs.ReadDir(fsys, ".")
	switch {
	case err != nil:
		return nil, []string{fmt.Sprintf("каталог миграций не читается: %v", err)}
	case len(entries) == 0:
		return nil, []string{"каталог миграций пуст"}
	}
	prev := 0
	for _, entry := range entries {
		name := entry.Name()
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			findings = append(findings, name+": имя не по форме 00001_имя.sql")
			continue
		}
		version, _ := strconv.Atoi(match[1]) // пять цифр — всегда число
		if version != prev+1 {
			findings = append(findings, fmt.Sprintf("%s: номер %s, ожидался %05d", name, match[1], prev+1))
		}
		prev = version
		raw, readErr := fs.ReadFile(fsys, name)
		if readErr != nil {
			findings = append(findings, fmt.Sprintf("%s: не читается: %v", name, readErr))
			continue
		}
		findings = append(findings, sectionFindings(name, string(raw))...)
		names = append(names, name)
	}
	return names, findings
}

// sectionFindings — по одной секции Up и Down, и Up раньше. Маркер — строка
// целиком, как в разборе pgtest: файл без Down goose принимает молча, и откат
// на стенде ничего не снимет.
func sectionFindings(name, text string) []string {
	var findings []string
	at := map[string]int{}
	for i, line := range strings.Split(text, "\n") {
		marker := strings.TrimSpace(line)
		if marker != pgtest.GooseUpMarker && marker != pgtest.GooseDownMarker {
			continue
		}
		if _, seen := at[marker]; seen {
			findings = append(findings, fmt.Sprintf("%s: секция %q повторяется", name, marker))
		}
		at[marker] = i
	}
	for _, marker := range []string{pgtest.GooseUpMarker, pgtest.GooseDownMarker} {
		if _, ok := at[marker]; !ok {
			findings = append(findings, fmt.Sprintf("%s: нет секции %q", name, marker))
		}
	}
	up, hasUp := at[pgtest.GooseUpMarker]
	if down, hasDown := at[pgtest.GooseDownMarker]; hasUp && hasDown && down < up {
		findings = append(findings, fmt.Sprintf("%s: секция %q раньше %q",
			name, pgtest.GooseDownMarker, pgtest.GooseUpMarker))
	}
	return findings
}

// schemaObjects — отношения, функции и типы текущей схемы пула. Пусто после
// отката — откат снял всё: DROP TABLE функцию триггера не снимает, а
// идемпотентный Up забытую функцию повторным накатом уже не выдаст.
//
// TODO(ADR-0011): pgtest.SchemaObjects после тега postgres.
func schemaObjects(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var schema *string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema))
	// search_path в несуществующую схему даёт NULL, и выборка ниже молча
	// ответила бы «пусто» при любом содержимом базы.
	require.NotNil(t, schema, "у пула нет текущей схемы")
	rows, err := pool.Query(t.Context(), schemaObjectsSQL, *schema)
	require.NoError(t, err)
	objects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return objects
}

const schemaObjectsSQL = `
SELECT 'отношение ' || c.relname FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1
UNION ALL
SELECT 'функция ' || p.proname FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1
UNION ALL
SELECT 'тип ' || t.typname FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = $1`

// Страж каталога на срабатывание: на каждом испорченном каталоге — находка с
// именем файла, на годном — ни одной.
func TestCatalogFindings_Fires(t *testing.T) {
	t.Parallel()

	const upDown = "-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n"
	file := func(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }
	tests := []struct {
		name string
		fsys fstest.MapFS
		want []string
	}{
		{name: "годный", fsys: fstest.MapFS{"00001_a.sql": file(upDown), "00002_b.sql": file(upDown)}},
		{
			name: "дубль номера", fsys: fstest.MapFS{"00001_a.sql": file(upDown), "00001_b.sql": file(upDown)},
			want: []string{"00001_b.sql: номер 00001, ожидался 00002"},
		},
		{
			name: "пропуск номера", fsys: fstest.MapFS{"00001_a.sql": file(upDown), "00003_b.sql": file(upDown)},
			want: []string{"00003_b.sql: номер 00003, ожидался 00002"},
		},
		{
			name: "нулевой номер", fsys: fstest.MapFS{"00000_a.sql": file(upDown)},
			want: []string{"00000_a.sql: номер 00000, ожидался 00001"},
		},
		{
			name: "файл без номера", fsys: fstest.MapFS{"00001_a.sql": file(upDown), "payment_b.sql": file(upDown)},
			want: []string{"payment_b.sql: имя не по форме 00001_имя.sql"},
		},
		{
			name: "расширение не .sql", fsys: fstest.MapFS{"00001_a.sql": file(upDown), "00002_b.SQL": file(upDown)},
			want: []string{"00002_b.SQL: имя не по форме 00001_имя.sql"},
		},
		{
			name: "без Down", fsys: fstest.MapFS{"00001_a.sql": file("-- +goose Up\nSELECT 1;\n")},
			want: []string{`00001_a.sql: нет секции "-- +goose Down"`},
		},
		{
			name: "Down раньше Up",
			fsys: fstest.MapFS{"00001_a.sql": file("-- +goose Down\nSELECT 1;\n-- +goose Up\nSELECT 1;\n")},
			want: []string{`00001_a.sql: секция "-- +goose Down" раньше "-- +goose Up"`},
		},
		{
			name: "секция повторяется", fsys: fstest.MapFS{"00001_a.sql": file("-- +goose Up\nSELECT 1;\n" + upDown)},
			want: []string{`00001_a.sql: секция "-- +goose Up" повторяется`},
		},
		{name: "пустой каталог", fsys: fstest.MapFS{}, want: []string{"каталог миграций пуст"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, findings := catalogFindings(tt.fsys)
			assert.Equal(t, tt.want, findings)
		})
	}
}
