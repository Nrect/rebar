package pgtest

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/postgres"
)

// GooseUpMarker — маркер секции наката в файле схемы.
const GooseUpMarker = "-- +goose Up"

// GooseDownMarker — маркер обратной секции. Вместе с GooseUpMarker это
// единственные директивы, переключающие секцию.
const GooseDownMarker = "-- +goose Down"

// Schema — своя схема на тест и пул с search_path в неё: параллельные тесты
// не видят строк друг друга, а имена таблиц в SQL остаются без префикса схемы.
// Схема не удаляется — базу прогона снимает Close.
func Schema(t *testing.T, db *DB) *pgxpool.Pool {
	t.Helper()
	name := createSchema(t, db)
	pool, err := newPool(t.Context(), db.DSN(), db.maxConns, name)
	if err != nil {
		t.Fatalf("pgtest: пул схемы %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SchemaDSN — та же схема на тест, но СТРОКОЙ СОЕДИНЕНИЯ: приложение поднимает
// пул само, это его нормальная форма, и собирать search_path руками ему не за
// чем. Изоляция та же, что у Schema, уборка та же: своего пула здесь нет, а
// схему снимает Close вместе с базой прогона.
func SchemaDSN(t *testing.T, db *DB) string {
	t.Helper()
	name := createSchema(t, db)
	// Через WithRuntimeParam, а не склейкой: он ещё и проверяет, что параметр
	// доехал именно до RuntimeParams — у DSN две формы, и в keyword/value
	// склейка «?search_path=» молча не сработала бы. Текст DSN в ошибку не
	// попадает: в нём пароль.
	dsn, err := postgres.WithRuntimeParam(db.DSN(), "search_path", name)
	if err != nil {
		t.Fatalf("pgtest: search_path=%s в DSN: %v", name, err)
	}
	return dsn
}

// createSchema — схема прогона со случайным именем; общий шаг Schema и
// SchemaDSN. Имя генерируется здесь и состоит из [a-z0-9]: имя схемы в DDL
// параметром не передать.
func createSchema(t *testing.T, db *DB) string {
	t.Helper()
	suffix, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	name := "t" + suffix
	if _, err = db.Pool().Exec(t.Context(), "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("pgtest: CREATE SCHEMA %s: %v", name, err)
	}
	return name
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
// ровно текст секции. StatementBegin/End внутри секции сохраняются вместе с
// телом функции: схема с триггером неизменяемости иначе применялась бы без
// самого триггера, а тест на append-only зеленел бы впустую.
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
		directive := strings.TrimSpace(line)
		if !strings.HasPrefix(directive, "-- +goose") {
			if inside {
				out.WriteString(line)
				out.WriteString("\n")
			}
			continue
		}
		// СЕКЦИЮ ПЕРЕКЛЮЧАЮТ ТОЛЬКО Up И Down. Прочие директивы goose
		// (StatementBegin/StatementEnd вокруг тела функции, NO TRANSACTION)
		// — часть секции: считая их сменой секции, разбор терял тело функции
		// триггера, схема применялась без него, и тест на append-only зеленел
		// на схеме, где триггера нет.
		if directive == GooseUpMarker || directive == GooseDownMarker {
			inside = directive == marker
			ok = ok || inside
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
