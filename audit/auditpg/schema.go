package auditpg

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Schema — содержимое schema.sql (с маркерами goose) для потребителя, который
// применяет миграции из кода, а не копирует файл. Побайтно равен файлу.
//
//go:embed schema.sql
var Schema string

const (
	tableName = "audit_events"

	// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
	missingTableHint = "auditpg.CheckSchema: таблицы audit_events нет: скопируйте auditpg/schema.sql в миграции"
	mismatchHint     = "auditpg.CheckSchema: таблица audit_events расходится с auditpg/schema.sql — сверьте миграцию"
)

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeTimestamptz = "timestamp with time zone"
)

// expectedColumns — колонки и их data_type из information_schema.columns;
// меняется только вместе со schema.sql (страж — TestExpectedColumns_MatchSchemaFile).
var expectedColumns = map[string]string{
	"id":          "uuid",
	"occurred_at": typeTimestamptz,
	"action":      typeText,
	"outcome":     typeText,
	"actor_kind":  typeText,
	"actor_id":    typeText,
	"actor_name":  typeText,
	"target_type": typeText,
	"target_id":   typeText,
	"request_id":  typeText,
	"ip":          typeText,
	"details":     "jsonb",
}

// expectedChecks — именованные CHECK: без них база не держит закрытые наборы.
var expectedChecks = []string{"audit_events_outcome_chk", "audit_events_actor_kind_chk"}

// expectedIndexes — имя индекса → обязан ли быть уникальным.
var expectedIndexes = map[string]bool{
	"ix_audit_events_occurred_at": false,
	"ix_audit_events_actor":       false,
	"ix_audit_events_target":      false,
}

// expectedTriggers — append-only держит база, поэтому пропавший триггер это
// расхождение схемы, а не мелочь оформления.
var expectedTriggers = []string{"audit_events_append_only_trg"}

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass('audit_events')`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

const checksSQL = `SELECT conname FROM pg_constraint WHERE conrelid = $1 AND contype = 'c'`

const indexesSQL = `SELECT indexname, indexdef LIKE 'CREATE UNIQUE INDEX %'
FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

const triggersSQL = `SELECT tgname FROM pg_trigger WHERE tgrelid = $1 AND NOT tgisinternal`

// CheckSchema сверяет таблицу audit_events со schema.sql, ничего не меняя:
// колонки и их типы, CHECK-ограничения, индексы и триггер неизменяемости.
// Зовётся на старте потребителя: миграцию применяет он сам, пакет только
// проверяет. Лишние колонки потребителя расхождением не считаются.
//
// Все расхождения — в одной ошибке (errors.Join), первая строка — что делать.
// Сбой запроса к каталогу — audit.ErrUnavailable. Данных таблицы в ошибке нет.
func (s *Sink) CheckSchema(ctx context.Context) error {
	var (
		oid    uint32
		schema string
	)
	err := s.db.QueryRow(ctx, tableSQL).Scan(&oid, &schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New(missingTableHint)
	}
	if err != nil {
		return storeError("check schema", err)
	}

	var problems []string
	for _, check := range []func(context.Context) ([]string, error){
		func(ctx context.Context) ([]string, error) { return s.checkColumns(ctx, schema) },
		func(ctx context.Context) ([]string, error) { return s.checkConstraints(ctx, oid) },
		func(ctx context.Context) ([]string, error) { return s.checkIndexes(ctx, schema) },
		func(ctx context.Context) ([]string, error) { return s.checkTriggers(ctx, oid) },
	} {
		found, checkErr := check(ctx)
		if checkErr != nil {
			return checkErr
		}
		problems = append(problems, found...)
	}
	if len(problems) == 0 {
		return nil
	}
	errs := make([]error, 0, len(problems)+1)
	errs = append(errs, errors.New(mismatchHint))
	for _, p := range problems {
		errs = append(errs, errors.New(p))
	}
	return errors.Join(errs...)
}

func (s *Sink) checkColumns(ctx context.Context, schema string) ([]string, error) {
	actual, err := queryPairs[string](ctx, s.db, columnsSQL, schema, tableName)
	if err != nil {
		return nil, storeError("check schema: columns", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(expectedColumns)) {
		got, ok := actual[name]
		if !ok {
			problems = append(problems, "колонки "+name+" нет")
			continue
		}
		if got != expectedColumns[name] {
			problems = append(problems, fmt.Sprintf("колонка %s: тип %s, ожидается %s", name, got, expectedColumns[name]))
		}
	}
	return problems, nil
}

func (s *Sink) checkConstraints(ctx context.Context, oid uint32) ([]string, error) {
	actual, err := queryNames(ctx, s.db, checksSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	var problems []string
	for _, name := range expectedChecks {
		if !slices.Contains(actual, name) {
			problems = append(problems, "ограничения "+name+" нет")
		}
	}
	return problems, nil
}

func (s *Sink) checkIndexes(ctx context.Context, schema string) ([]string, error) {
	actual, err := queryPairs[bool](ctx, s.db, indexesSQL, schema, tableName)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(expectedIndexes)) {
		unique, ok := actual[name]
		if !ok {
			problems = append(problems, "индекса "+name+" нет")
			continue
		}
		if expectedIndexes[name] && !unique {
			problems = append(problems, "индекс "+name+" не уникальный")
		}
	}
	return problems, nil
}

func (s *Sink) checkTriggers(ctx context.Context, oid uint32) ([]string, error) {
	actual, err := queryNames(ctx, s.db, triggersSQL, oid)
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	var problems []string
	for _, name := range expectedTriggers {
		if !slices.Contains(actual, name) {
			problems = append(problems, "триггера "+name+" нет: журнал перестал быть append-only")
		}
	}
	return problems, nil
}

// queryNames — один столбец имён в срез.
func queryNames(ctx context.Context, db executor, sql string, args ...any) ([]string, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// queryPairs — строки «имя, значение» в карту.
func queryPairs[V any](ctx context.Context, db executor, sql string, args ...any) (map[string]V, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]V{}
	for rows.Next() {
		var (
			name  string
			value V
		)
		if scanErr := rows.Scan(&name, &value); scanErr != nil {
			return nil, scanErr
		}
		out[name] = value
	}
	return out, rows.Err()
}
