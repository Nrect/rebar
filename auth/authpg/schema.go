package authpg

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/postgres"
)

// Schema — содержимое schema.sql (с маркерами goose) для потребителя, который
// применяет миграции из кода, а не копирует файл. Побайтно равен файлу.
//
//go:embed schema.sql
var Schema string

// colRealm — имя колонки реалма: она есть во всех трёх таблицах, и в этом
// смысл — реалм входит в ключ строки и в каждый WHERE.
const colRealm = "realm"

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeTimestamptz = "timestamp with time zone"
	typeUUID        = "uuid"
)

// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
const (
	missingTableHint = "authpg.CheckSchema: таблицы %s нет: скопируйте authpg/schema.sql в миграции"
	mismatchHint     = "authpg.CheckSchema: схема расходится с authpg/schema.sql — сверьте миграцию"
)

// tableSpec — таблица так, как её видит каталог Postgres. Меняется только
// вместе со schema.sql; страж — TestExpectedSchema_MatchesFile.
type tableSpec struct {
	name    string
	columns map[string]string
	// checks — ИМЕНОВАННЫЕ ограничения. Безымянные CHECK колонок (realm,
	// purpose, длина user_agent) проверяет guard-тест по файлу схемы: их имена
	// генерирует Postgres, и контрактом они быть не могут.
	checks []string
	// indexes — имя индекса → обязан ли быть уникальным.
	indexes map[string]bool
}

var expectedTables = []tableSpec{
	{
		name: tableSessions,
		columns: map[string]string{
			"token_hash": typeText, colRealm: typeText, "subject_id": typeUUID,
			"created_at": typeTimestamptz, "last_seen_at": typeTimestamptz,
			"expires_at": typeTimestamptz, "idle_expires_at": typeTimestamptz,
			"ip": typeText, "user_agent": typeText,
		},
		checks: []string{"auth_sessions_idle_chk"},
		indexes: map[string]bool{
			"auth_sessions_pkey":       true,
			"ix_auth_sessions_subject": false,
			"ix_auth_sessions_expires": false,
		},
	},
	{
		name: tableTokens,
		columns: map[string]string{
			"token_hash": typeText, colRealm: typeText, "purpose": typeText,
			"subject_id": typeUUID, "payload": typeText,
			"expires_at": typeTimestamptz, "created_at": typeTimestamptz, "used_at": typeTimestamptz,
		},
		indexes: map[string]bool{
			// Первичный ключ — он же гарантия одноразовости: без него один
			// сырой токен ложится двумя строками и гасится дважды.
			"auth_tokens_pkey":       true,
			"ix_auth_tokens_subject": false,
			"ix_auth_tokens_expires": false,
		},
	},
	{
		name: tableAttempts,
		columns: map[string]string{
			"id": typeUUID, colRealm: typeText, "login_key": typeText,
			"ip": typeText, "at": typeTimestamptz,
		},
		indexes: map[string]bool{
			"auth_login_attempts_pkey":   true,
			"ix_auth_login_attempts_key": false,
			"ix_auth_login_attempts_at":  false,
		},
	},
}

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass($1::text)`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

const checksSQL = `SELECT conname FROM pg_constraint WHERE conrelid = $1 AND contype = 'c'`

const indexesSQL = `SELECT indexname, indexdef LIKE 'CREATE UNIQUE INDEX %'
FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

// CheckSchema сверяет три таблицы со schema.sql, НИЧЕГО НЕ МЕНЯЯ: колонки и их
// типы, именованные CHECK, индексы. Зовётся на старте потребителя: миграцию он
// применяет своим раннером — автомиграция из библиотеки даёт две правды о
// схеме, требует DDL-прав у приложения и гонку реплик при выкате
// (CONVENTIONS §9).
//
// Все расхождения — в одной ошибке (errors.Join), первая строка — что делать.
// Сбой запроса к каталогу — auth.ErrUnavailable. Данных таблиц в ошибке нет.
func (s *Store) CheckSchema(ctx context.Context) error {
	var problems []string
	for _, spec := range expectedTables {
		found, err := s.checkTable(ctx, spec)
		if err != nil {
			return err
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

func (s *Store) checkTable(ctx context.Context, spec tableSpec) ([]string, error) {
	var (
		oid    uint32
		schema string
	)
	err := s.db.QueryRow(ctx, tableSQL, spec.name).Scan(&oid, &schema)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf(missingTableHint, spec.name)
	case err != nil:
		return nil, storeError("check schema", err)
	}

	var problems []string
	for _, check := range []func() ([]string, error){
		func() ([]string, error) { return s.checkColumns(ctx, schema, spec) },
		func() ([]string, error) { return s.checkConstraints(ctx, oid, spec) },
		func() ([]string, error) { return s.checkIndexes(ctx, schema, spec) },
	} {
		found, checkErr := check()
		if checkErr != nil {
			return nil, checkErr
		}
		problems = append(problems, found...)
	}
	return problems, nil
}

func (s *Store) checkColumns(ctx context.Context, schema string, spec tableSpec) ([]string, error) {
	actual, err := queryPairs[string](ctx, s.db, columnsSQL, schema, spec.name)
	if err != nil {
		return nil, storeError("check schema: columns", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(spec.columns)) {
		switch got, ok := actual[name]; {
		case !ok:
			problems = append(problems, spec.name+": колонки "+name+" нет")
		case got != spec.columns[name]:
			problems = append(problems,
				fmt.Sprintf("%s: колонка %s: тип %s, ожидается %s", spec.name, name, got, spec.columns[name]))
		}
	}
	return problems, nil
}

func (s *Store) checkConstraints(ctx context.Context, oid uint32, spec tableSpec) ([]string, error) {
	if len(spec.checks) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, checksSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	defer rows.Close()
	actual := map[string]bool{}
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, storeError("check schema: constraints", scanErr)
		}
		actual[name] = true
	}
	if rows.Err() != nil {
		return nil, storeError("check schema: constraints", rows.Err())
	}
	var problems []string
	for _, name := range spec.checks {
		if !actual[name] {
			problems = append(problems, spec.name+": ограничения "+name+" нет")
		}
	}
	return problems, nil
}

func (s *Store) checkIndexes(ctx context.Context, schema string, spec tableSpec) ([]string, error) {
	actual, err := queryPairs[bool](ctx, s.db, indexesSQL, schema, spec.name)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(spec.indexes)) {
		switch unique, ok := actual[name]; {
		case !ok:
			problems = append(problems, spec.name+": индекса "+name+" нет")
		case spec.indexes[name] && !unique:
			problems = append(problems, spec.name+": индекс "+name+" не уникальный")
		}
	}
	return problems, nil
}

// queryPairs — строки «имя, значение» в карту.
func queryPairs[V any](ctx context.Context, db postgres.Querier, sql string, args ...any) (map[string]V, error) {
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
