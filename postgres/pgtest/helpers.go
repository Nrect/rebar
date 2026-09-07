package pgtest

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GooseUpMarker — маркер секции наката в файле схемы.
const GooseUpMarker = "-- +goose Up"

// Schema — своя схема на тест и пул с search_path в неё: параллельные тесты
// не видят строк друг друга, а имена таблиц в SQL остаются без префикса схемы.
// Схема не удаляется — базу прогона снимает Close.
func Schema(t *testing.T, db *DB) *pgxpool.Pool {
	t.Helper()
	suffix, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	name := "t" + suffix
	if _, err = db.Pool().Exec(t.Context(), "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("pgtest: CREATE SCHEMA %s: %v", name, err)
	}
	pool, err := newPool(t.Context(), db.DSN(), defaultMaxConns, name)
	if err != nil {
		t.Fatalf("pgtest: пул схемы %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Apply выполняет SQL в пуле теста — например тело Up из schema.sql.
func Apply(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("pgtest: применение SQL: %v", err)
	}
}

// GooseUp — тело секции «-- +goose Up» файла миграции.
//
// БЕЗ ЗАВИСИМОСТИ ОТ GOOSE: раннер миграций — дело потребителя, а тесту нужен
// ровно текст секции. StatementBegin/End не поддерживаются: в схеме пакета нет
// тел функций с ';' внутри.
func GooseUp(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // путь к схеме даёт сам тест
	if err != nil {
		t.Fatalf("pgtest: чтение %s: %v", path, err)
	}
	body, ok := gooseSection(string(raw), GooseUpMarker)
	if !ok {
		t.Fatalf("pgtest: в %s нет маркера %q", path, GooseUpMarker)
	}
	return body
}

// gooseSection — строки между маркером и следующей директивой «-- +goose» или
// концом файла; ok = false, если маркера нет.
func gooseSection(sql, marker string) (body string, ok bool) {
	var out strings.Builder
	inside := false
	for line := range strings.SplitSeq(sql, "\n") {
		if directive := strings.TrimSpace(line); strings.HasPrefix(directive, "-- +goose") {
			inside = directive == marker
			ok = ok || inside
			continue
		}
		if inside {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String(), ok
}

// Now — момент так, как его хранит timestamptz: UTC и микросекунды.
// Наносекунды Go база теряет, и сравнение прочитанного с исходным без этого
// усечения всегда красное.
func Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Short — пропуск теста, которому нужна база. TestMain обязан проверить
// testing.Short() раньше и выйти, не поднимая Docker (см. doc.go).
func Short(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker или " + EnvDatabaseURL)
	}
}
