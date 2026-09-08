package authzpg

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
	tableName = "authz_role_assignments"

	// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
	missingTableHint = "authzpg.CheckSchema: таблицы authz_role_assignments нет: скопируйте authzpg/schema.sql в миграции"
	mismatchHint     = "authzpg.CheckSchema: таблица authz_role_assignments расходится с authzpg/schema.sql — сверьте миграцию"
)

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeTimestamptz = "timestamp with time zone"
)

// expectedColumns — колонки и их data_type; меняется только вместе со
// schema.sql (страж — TestExpectedColumns_MatchSchemaFile).
var expectedColumns = map[string]string{
	"realm":      typeText,
	"subject_id": typeText,
	"role":       typeText,
	"granted_by": typeText,
	"granted_at": typeTimestamptz,
	"expires_at": typeTimestamptz,
}

// expectedConstraints — именованные ограничения. Первичный ключ здесь не для
// красоты: ON CONFLICT ON CONSTRAINT в Assign ссылается на него ПО ИМЕНИ,
// поэтому переименование в миграции потребителя ломало бы выдачу роли молча.
var expectedConstraints = []string{
	"authz_role_assignments_pkey",
	"authz_role_assignments_realm_chk",
	"authz_role_assignments_subject_chk",
	"authz_role_assignments_role_chk",
	"authz_role_assignments_granted_by_chk",
	"authz_role_assignments_expires_chk",
}

// expectedIndexes — имя индекса → обязан ли быть уникальным.
var expectedIndexes = map[string]bool{
	"authz_role_assignments_pkey":       true,  // он же арбитр ON CONFLICT
	"ix_authz_role_assignments_expires": false, // уборка истёкших
}

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass('authz_role_assignments')`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

const constraintsSQL = `SELECT conname FROM pg_constraint WHERE conrelid = $1 AND contype IN ('c', 'p')`

const indexesSQL = `SELECT indexname, indexdef LIKE 'CREATE UNIQUE INDEX %'
FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

// CheckSchema сверяет таблицу со schema.sql, ничего не меняя: колонки и их
// типы, ограничения, индексы. Зовётся на старте потребителя: миграцию
// применяет он сам, пакет только проверяет. Автомиграция из библиотеки дала
// бы две правды о схеме, требовала DDL-прав у приложения и гонку реплик при
// выкате. Лишние колонки потребителя расхождением не считаются.
//
// Все расхождения — в одной ошибке (errors.Join), первая строка — что делать.
// Сбой запроса к каталогу — authz.ErrUnavailable. Данных таблицы в ошибке нет.
func (s *Store) CheckSchema(ctx context.Context) error {
	var (
		oid    uint32
		schema string
	)
	err := s.db.QueryRow(ctx, tableSQL).Scan(&oid, &schema)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errors.New(missingTableHint)
	case err != nil:
		return storeError("check schema", err)
	}

	var problems []string
	for _, check := range []func(context.Context) ([]string, error){
		func(ctx context.Context) ([]string, error) { return s.checkColumns(ctx, schema) },
		func(ctx context.Context) ([]string, error) { return s.checkConstraints(ctx, oid) },
		func(ctx context.Context) ([]string, error) { return s.checkIndexes(ctx, schema) },
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

func (s *Store) checkColumns(ctx context.Context, schema string) ([]string, error) {
	actual, err := queryPairs[string](ctx, s.db, columnsSQL, schema, tableName)
	if err != nil {
		return nil, storeError("check schema: columns", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(expectedColumns)) {
		switch got, ok := actual[name]; {
		case !ok:
			problems = append(problems, "колонки "+name+" нет")
		case got != expectedColumns[name]:
			problems = append(problems, fmt.Sprintf("колонка %s: тип %s, ожидается %s", name, got, expectedColumns[name]))
		}
	}
	return problems, nil
}

func (s *Store) checkConstraints(ctx context.Context, oid uint32) ([]string, error) {
	rows, err := s.db.Query(ctx, constraintsSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	actual, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	var problems []string
	for _, name := range expectedConstraints {
		if !slices.Contains(actual, name) {
			problems = append(problems, "ограничения "+name+" нет")
		}
	}
	return problems, nil
}

func (s *Store) checkIndexes(ctx context.Context, schema string) ([]string, error) {
	actual, err := queryPairs[bool](ctx, s.db, indexesSQL, schema, tableName)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(expectedIndexes)) {
		switch unique, ok := actual[name]; {
		case !ok:
			problems = append(problems, "индекса "+name+" нет")
		case expectedIndexes[name] && !unique:
			problems = append(problems, "индекс "+name+" не уникальный")
		}
	}
	return problems, nil
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
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}
