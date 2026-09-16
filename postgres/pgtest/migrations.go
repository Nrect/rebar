package pgtest

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationName — имя файла каталога: номер в пять знаков, как у
// `goose create -s`, подчёркивание, имя и .sql (ADR-0011, решение 1).
var migrationName = regexp.MustCompile(`^(\d{5})_.+\.sql$`)

// migration — файл каталога и тела его секций.
type migration struct {
	name, up, down string
}

// ApplyUp накатывает каталог миграций на пул теста: секции Up по возрастанию
// номера, файл — одним запросом.
//
// НЕ РАННЕР: таблицы версий нет, каталог накатывается целиком на свежую схему
// (Schema). Поэтому и версий не возвращает: полноту и порядок держит
// CheckMigrations, а тест проверяет схему, а не список файлов. Каталог с
// находками не накатывается — тест применил бы не ту последовательность, что
// goose у потребителя.
func ApplyUp(tb testing.TB, pool *pgxpool.Pool, fsys fs.FS) {
	tb.Helper()
	for _, m := range catalog(tb, fsys) {
		execSection(tb, pool, m.name, GooseUpMarker, m.up)
	}
}

// ApplyDown откатывает каталог: секции Down по убыванию номера. Down пишется
// идемпотентным (ADR-0011, уточнение 1), и повторный вызов обязан пройти.
func ApplyDown(tb testing.TB, pool *pgxpool.Pool, fsys fs.FS) {
	tb.Helper()
	files := catalog(tb, fsys)
	// Не range slices.Backward: тело range-over-func — замыкание мимо Helper, и падение указало бы сюда, а не на строку теста.
	slices.Reverse(files)
	for _, m := range files {
		execSection(tb, pool, m.name, GooseDownMarker, m.down)
	}
}

// CheckMigrations — стражи каталога (ADR-0011, решение 7, пункты 1–3): номера
// в пять знаков идут подряд с 00001 без повторов, файлов без номера нет, у
// каждого файла по одной секции Up и Down, и Up раньше. Каждая находка —
// отдельная ошибка теста с именем файла. База не нужна: идёт и под -short.
func CheckMigrations(tb testing.TB, fsys fs.FS) {
	tb.Helper()
	_, findings := readCatalog(fsys)
	for _, finding := range findings {
		tb.Errorf("pgtest: %s", finding)
	}
}

// SchemaObjects — объекты текущей схемы пула строками «род имя»: таблицы,
// индексы, последовательности, представления, функции и типы. Пусто после
// ApplyDown — откат снял всё.
//
// ФУНКЦИИ СЧИТАЮТСЯ НАРАВНЕ С ТАБЛИЦАМИ: DROP TABLE снимает индексы и триггеры,
// но не функцию триггера, а идемпотентный Up (CREATE OR REPLACE) забытую
// функцию повторным накатом уже не выдаст.
func SchemaObjects(tb testing.TB, pool *pgxpool.Pool) []string {
	tb.Helper()
	var schema *string
	if err := pool.QueryRow(tb.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		tb.Fatalf("pgtest: текущая схема: %v", err)
	}
	// search_path в несуществующую схему даёт NULL, и запрос ниже молча
	// ответил бы «пусто» при любом содержимом базы.
	if schema == nil {
		tb.Fatal("pgtest: у пула нет текущей схемы: search_path указывает в несуществующую")
	}
	rows, err := pool.Query(tb.Context(), schemaObjectsSQL, *schema)
	if err != nil {
		tb.Fatalf("pgtest: объекты схемы %s: %v", *schema, err)
	}
	objects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		tb.Fatalf("pgtest: объекты схемы %s: %v", *schema, err)
	}
	slices.Sort(objects) // порядок строк в базе зависит от её collation
	return objects
}

// schemaObjectsSQL — отношения, функции и типы схемы. Строчный тип отношения и
// тип массива уходят вместе с владельцем и не считаются; составной тип есть и
// в pg_class, и в pg_type — считается один раз, типом.
const schemaObjectsSQL = `
SELECT CASE c.relkind
           WHEN 'r' THEN 'таблица '
           WHEN 'p' THEN 'таблица '
           WHEN 'i' THEN 'индекс '
           WHEN 'I' THEN 'индекс '
           WHEN 'S' THEN 'последовательность '
           WHEN 'v' THEN 'представление '
           WHEN 'm' THEN 'представление '
           ELSE 'отношение '
       END || c.relname
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1 AND c.relkind <> 'c'
UNION ALL
SELECT 'функция ' || p.proname
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = $1
UNION ALL
SELECT 'тип ' || t.typname
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE n.nspname = $1
   AND (t.typrelid = 0 OR EXISTS (SELECT 1 FROM pg_class r WHERE r.oid = t.typrelid AND r.relkind = 'c'))
   AND NOT EXISTS (SELECT 1 FROM pg_type el WHERE el.oid = t.typelem AND el.typarray = t.oid)`

// catalog — файлы каталога для наката; при находках тест падает, не тронув базу.
func catalog(tb testing.TB, fsys fs.FS) []migration {
	tb.Helper()
	files, findings := readCatalog(fsys)
	if len(findings) > 0 {
		tb.Fatalf("pgtest: каталог миграций не применяется:\n%s", strings.Join(findings, "\n"))
	}
	return files
}

// execSection — секция одним запросом: без аргументов pgx идёт простым
// протоколом, и команды файла проходят одной неявной транзакцией, как миграция
// у goose.
func execSection(tb testing.TB, pool *pgxpool.Pool, name, marker, sql string) {
	tb.Helper()
	if _, err := pool.Exec(tb.Context(), sql); err != nil {
		tb.Fatalf("pgtest: %s, секция %q: %v", name, marker, err)
	}
}

// readCatalog — файлы каталога по возрастанию номера и находки стражей.
// ReadDir отдаёт записи по имени, а номер фиксированной ширины делает порядок
// имён порядком номеров.
func readCatalog(fsys fs.FS) (files []migration, findings []string) {
	entries, err := fs.ReadDir(fsys, ".")
	switch {
	case err != nil:
		return nil, []string{fmt.Sprintf("каталог миграций не читается: %v", err)}
	case len(entries) == 0:
		return nil, []string{"каталог миграций пуст"}
	}

	var seq sequence
	for _, entry := range entries {
		name := entry.Name()
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			findings = append(findings, name+": имя не по форме 00001_имя.sql")
			continue
		}
		if finding := seq.next(name, match[1]); finding != "" {
			findings = append(findings, finding)
		}
		raw, readErr := fs.ReadFile(fsys, name)
		if readErr != nil {
			findings = append(findings, fmt.Sprintf("%s: не читается: %v", name, readErr))
			continue
		}
		text := string(raw)
		findings = append(findings, sectionFindings(name, text)...)
		up, _ := gooseSection(text, GooseUpMarker)
		down, _ := gooseSection(text, GooseDownMarker)
		files = append(files, migration{name: name, up: up, down: down})
	}
	return files, findings
}

// sequence — страж номеров: подряд с 00001, без повторов. Дубль goose
// отвергает сам, но только на старте у потребителя; пропуск прячет потерянный
// файл, а нулевой номер goose молча пропускает.
type sequence struct {
	prevName    string
	prevVersion int
}

// next — находка по очередному номеру или "".
func (s *sequence) next(name, number string) string {
	version, _ := strconv.Atoi(number) // пять цифр — всегда число
	if s.prevName != "" && version == s.prevVersion {
		return name + ": номер " + number + " уже занят файлом " + s.prevName
	}
	expected := s.prevVersion + 1
	s.prevName, s.prevVersion = name, version
	if version != expected {
		return fmt.Sprintf("%s: номер %s, ожидался %05d", name, number, expected)
	}
	return ""
}

// sectionFindings — по одной секции Up и Down, и Up раньше. Повтор и Down до
// Up goose отвергает сам, а файл без Down принимает молча — тогда откат на
// стенде ничего не снимет. Маркер узнаётся так же, как в gooseSection.
func sectionFindings(name, text string) []string {
	counts := map[string]int{}
	downFirst := false
	for line := range strings.SplitSeq(text, "\n") {
		marker := strings.TrimSpace(line)
		if marker != GooseUpMarker && marker != GooseDownMarker {
			continue
		}
		counts[marker]++
		downFirst = downFirst || marker == GooseDownMarker && counts[GooseUpMarker] == 0
	}

	var findings []string
	for _, marker := range []string{GooseUpMarker, GooseDownMarker} {
		switch n := counts[marker]; {
		case n == 0:
			findings = append(findings, fmt.Sprintf("%s: нет секции %q", name, marker))
		case n > 1:
			findings = append(findings, fmt.Sprintf("%s: секция %q повторяется", name, marker))
		}
	}
	if downFirst && counts[GooseUpMarker] > 0 {
		findings = append(findings, fmt.Sprintf("%s: секция %q раньше %q", name, GooseDownMarker, GooseUpMarker))
	}
	return findings
}
