package pgtest_test

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres"
	"github.com/nrect/rebar/postgres/pgtest"
)

//go:embed testdata/migrations/good/*.sql
var goodFiles embed.FS

// goodCatalog — учебный каталог в той же форме, что Migrations() блока: fs.Sub
// от embed.
func goodCatalog(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(goodFiles, "testdata/migrations/good")
	require.NoError(t, err)
	return sub
}

// corpus — каталог из testdata/migrations.
func corpus(name string) fs.FS {
	return os.DirFS(filepath.Join("testdata", "migrations", name))
}

// sqlFile — файл каталога из MapFS.
func sqlFile(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }

// upDown — годный файл для каталогов из MapFS.
const upDown = "-- +goose Up\nSELECT 1;\n\n-- +goose Down\nSELECT 1;\n"

// Стражи на срабатывание: на годном каталоге ноль находок, на каждом
// испорченном — ровно ожидаемая, с именем файла. Каталоги из testdata — случаи
// ADR-0011, решение 7; MapFS — края тех же правил.
func TestCheckMigrations_Fires(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fs.FS
		want []string
	}{
		{name: "годный каталог", fsys: corpus("good")},
		{
			name: "дубль номера", fsys: corpus("duplicate"),
			want: []string{"pgtest: 00002_note_index.sql: номер 00002 уже занят файлом 00002_note_feed.sql"},
		},
		{
			name: "пропуск номера", fsys: corpus("gap"),
			want: []string{"pgtest: 00003_note_feed.sql: номер 00003, ожидался 00002"},
		},
		{
			name: "файл без номера", fsys: corpus("unnumbered"),
			want: []string{"pgtest: note_index.sql: имя не по форме 00001_имя.sql"},
		},
		{
			name: "файл без Down", fsys: corpus("no_down"),
			want: []string{`pgtest: 00002_note_index.sql: нет секции "-- +goose Down"`},
		},
		{
			name: "номер в четыре знака",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile(upDown), "0002_b.sql": sqlFile(upDown)},
			want: []string{"pgtest: 0002_b.sql: имя не по форме 00001_имя.sql"},
		},
		{
			name: "расширение не .sql",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile(upDown), "00002_b.SQL": sqlFile(upDown)},
			want: []string{"pgtest: 00002_b.SQL: имя не по форме 00001_имя.sql"},
		},
		{
			name: "нумерация не с единицы",
			fsys: fstest.MapFS{"00002_a.sql": sqlFile(upDown)},
			want: []string{"pgtest: 00002_a.sql: номер 00002, ожидался 00001"},
		},
		{
			name: "нулевой номер",
			fsys: fstest.MapFS{"00000_a.sql": sqlFile(upDown), "00001_b.sql": sqlFile(upDown)},
			want: []string{"pgtest: 00000_a.sql: номер 00000, ожидался 00001"},
		},
		{
			name: "без Up",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile("-- +goose Down\nSELECT 1;\n")},
			want: []string{`pgtest: 00001_a.sql: нет секции "-- +goose Up"`},
		},
		{
			name: "секция повторяется",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile("-- +goose Up\nSELECT 1;\n" + upDown)},
			want: []string{`pgtest: 00001_a.sql: секция "-- +goose Up" повторяется`},
		},
		{
			name: "Down раньше Up",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile("-- +goose Down\nSELECT 1;\n-- +goose Up\nSELECT 1;\n")},
			want: []string{`pgtest: 00001_a.sql: секция "-- +goose Down" раньше "-- +goose Up"`},
		},
		{
			// goose такой маркер примет, а разбор секций — нет: тест применил
			// бы Down вместе с Up.
			name: "маркер не в том регистре",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile("-- +goose Up\nSELECT 1;\n-- +goose down\nSELECT 1;\n")},
			want: []string{`pgtest: 00001_a.sql: нет секции "-- +goose Down"`},
		},
		{
			name: "каталог под именем миграции",
			fsys: fstest.MapFS{"00001_a.sql": sqlFile(upDown), "00002_b.sql/00001_c.sql": sqlFile(upDown)},
			want: []string{"pgtest: 00002_b.sql: не читается: read 00002_b.sql: invalid argument"},
		},
		{name: "пустой каталог", fsys: fstest.MapFS{}, want: []string{"pgtest: каталог миграций пуст"}},
		{
			// fs.Sub с опечаткой в Migrations(): иначе страж молчал бы на
			// каталоге, которого нет.
			name: "каталог не читается",
			fsys: mustSub(t, fstest.MapFS{"migrations/00001_a.sql": sqlFile(upDown)}, "migrationz"),
			want: []string{"pgtest: каталог миграций не читается: open .: file does not exist"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tb := &fakeTB{}
			require.Nil(t, run(func() { pgtest.CheckMigrations(tb, tt.fsys) }))
			assert.Equal(t, tt.want, tb.errors)
			assert.Empty(t, tb.fatal)
		})
	}
}

func mustSub(t *testing.T, fsys fs.FS, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(fsys, dir)
	require.NoError(t, err)
	return sub
}

// Каталог с находками не накатывается и не откатывается: помощник падает
// раньше, чем тронет пул. Пул — nil: тронь его помощник, была бы паника.
func TestApplyUpDown_RefuseBadCatalog(t *testing.T) {
	t.Parallel()

	const want = "pgtest: каталог миграций не применяется:\n00003_note_feed.sql: номер 00003, ожидался 00002"
	for _, tt := range []struct {
		name  string
		apply func(testing.TB, *pgxpool.Pool, fs.FS)
	}{
		{name: "ApplyUp", apply: pgtest.ApplyUp},
		{name: "ApplyDown", apply: pgtest.ApplyDown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tb := &fakeTB{}
			require.Nil(t, run(func() { tt.apply(tb, nil, corpus("gap")) }), "помощник тронул пул")
			assert.Equal(t, want, tb.fatal)
		})
	}
}

// Учебный каталог на живой базе (ADR-0011, решение 7, пункт 4, и уточнение 1):
// накат на пустую схему, откат — схема пуста, повторный откат не падает.
// Порядок держат зависимости каталога: триггер из 00002 и представление из
// 00003 не создаются без таблицы из 00001, а она не снимается, пока живо
// представление.
func TestApplyUpDown_RoundTrip(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	pool := pgtest.Schema(t, db)
	catalog := goodCatalog(t)
	applied := []string{
		"индекс ix_note_created_at",
		"индекс note_pkey",
		"представление note_feed",
		"таблица note",
		"функция note_append_only",
	}

	pgtest.ApplyUp(t, pool, catalog)
	require.Equal(t, applied, pgtest.SchemaObjects(t, pool), "накат применил не весь каталог")

	// Повторный накат идемпотентен и оставляет триггер в ALWAYS (решение 4).
	pgtest.ApplyUp(t, pool, catalog)
	require.Equal(t, applied, pgtest.SchemaObjects(t, pool))
	var mode string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT tgenabled::text FROM pg_trigger WHERE tgrelid = 'note'::regclass AND tgname = 'note_append_only_trg'`,
	).Scan(&mode))
	assert.Equal(t, "A", mode)

	pgtest.ApplyDown(t, pool, catalog)
	assert.Empty(t, pgtest.SchemaObjects(t, pool), "откат оставил объекты")

	pgtest.ApplyDown(t, pool, catalog)
	assert.Empty(t, pgtest.SchemaObjects(t, pool), "повторный откат оставил объекты")
}

// SchemaObjects видит всё, что откат обязан снять, и не видит того, что уходит
// с владельцем: строчных типов отношений и типов массивов.
func TestSchemaObjects_SeesEveryKind(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	pool := pgtest.Schema(t, db)
	require.Empty(t, pgtest.SchemaObjects(t, pool), "свежая схема обязана быть пустой")

	pgtest.Apply(t, pool, `
		CREATE SEQUENCE counter;
		CREATE TYPE mood AS ENUM ('ok');
		CREATE TYPE pair AS (a INT, b INT);
		CREATE TABLE item (id INT);
		CREATE MATERIALIZED VIEW item_ids AS SELECT id FROM item;
		CREATE FUNCTION answer() RETURNS INT LANGUAGE sql AS 'SELECT 42';
	`)

	assert.Equal(t, []string{
		"последовательность counter",
		"представление item_ids",
		"таблица item",
		"тип mood",
		"тип pair",
		"функция answer",
	}, pgtest.SchemaObjects(t, pool))
}

// search_path в несуществующую схему: current_schema() — NULL, и без проверки
// SchemaObjects молча отвечал бы «пусто».
func TestSchemaObjects_NoCurrentSchema(t *testing.T) {
	t.Parallel()
	pgtest.Short(t)

	dsn, err := postgres.WithRuntimeParam(db.DSN(), "search_path", "pgtest_no_such_schema")
	require.NoError(t, err)
	pool := appPool(t, dsn)

	tb := &fakeTB{ctx: t.Context()}
	require.Nil(t, run(func() { pgtest.SchemaObjects(tb, pool) }))
	assert.Equal(t, "pgtest: у пула нет текущей схемы: search_path указывает в несуществующую", tb.fatal)
}

// fakeTB — подставной TB: копит сообщения вместо падения теста, иначе страж не
// проверить на срабатывание. Встроенный TB — nil: неподменённый метод
// паникует, и run вернёт панику значением.
type fakeTB struct {
	testing.TB

	ctx    context.Context
	errors []string
	fatal  string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Context() context.Context { return f.ctx }

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

// Fatal и Fatalf останавливают вызов, как настоящие, — через Goexit.
func (f *fakeTB) Fatal(args ...any) {
	f.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// run зовёт помощника в своей горутине: Goexit из Fatal завершает её, а не
// тест, а паника возвращается значением.
func run(fn func()) (panicked any) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		fn()
	}()
	<-done
	return panicked
}
