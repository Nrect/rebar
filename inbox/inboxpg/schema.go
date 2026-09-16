package inboxpg

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/postgres"
)

// Имена таблиц. Без имени схемы: search_path выбирает потребитель.
const (
	tableEvents   = "inbox_events"
	tablePayloads = "inbox_payloads"
)

// Колонки, общие у обеих таблиц.
const (
	colSource     = "source"
	colEventID    = "event_id"
	colReceivedAt = "received_at"
)

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeBytea       = "bytea"
	typeTimestamptz = "timestamp with time zone"
)

// fnAppendOnly — функция обоих триггеров; её имя — оно же имя отказа
// (RAISE … USING CONSTRAINT).
const fnAppendOnly = "inbox_append_only"

// tableSpec — что обязано быть у таблицы. Меняется только вместе с миграцией;
// страж расхождения — TestExpected_MatchesMigrations.
type tableSpec struct {
	columns map[string]string
	// constraints — первичный ключ, CHECK и внешний ключ по имени: ON CONFLICT
	// ON CONSTRAINT находит ключ дедупа по имени ограничения, а не индекса.
	constraints []string
	indexes     []string
	// triggers — триггер и его функция.
	triggers map[string]string
}

var expected = map[string]tableSpec{
	tableEvents: {
		columns: map[string]string{
			colSource: typeText, colEventID: typeText, "event_type": typeText, "digest": typeBytea,
			"occurred_at": typeTimestamptz, colReceivedAt: typeTimestamptz,
		},
		constraints: []string{
			"inbox_events_digest_chk", "inbox_events_id_chk", "inbox_events_occurred_chk",
			"inbox_events_source_chk", "inbox_events_type_chk", "ux_inbox_events_dedup",
		},
		indexes:  []string{"ix_inbox_events_received"},
		triggers: map[string]string{"inbox_events_append_only_trg": fnAppendOnly},
	},
	tablePayloads: {
		columns: map[string]string{
			colSource: typeText, colEventID: typeText, "payload": typeBytea, colReceivedAt: typeTimestamptz,
		},
		constraints: []string{"inbox_payloads_event_fkey", "inbox_payloads_size_chk", "ux_inbox_payloads_event"},
		indexes:     []string{"ix_inbox_payloads_received"},
		triggers:    map[string]string{"inbox_payloads_append_only_trg": fnAppendOnly},
	},
}

// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
const (
	missingTableHint = "inboxpg.CheckSchema: таблицы %s нет: накатите inboxpg.Migrations() раннером проекта"
	mismatchHint     = "inboxpg.CheckSchema: схема расходится с миграциями — накатите inboxpg.Migrations() раннером проекта; что осталось после наката, чините своей миграцией"
)

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass($1)`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

// Второе поле — годна ли форма: у внешнего ключа это ON DELETE CASCADE, без него
// тело пережило бы отметку.
const constraintsSQL = `SELECT conname, contype <> 'f' OR confdeltype = 'c'
FROM pg_constraint WHERE conrelid = $1 AND contype IN ('c', 'f', 'p')`

const indexesSQL = `SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

// tgenabled = 'A' — ENABLE ALWAYS: в режиме по умолчанию ('O') триггер молчит
// при репликации, а DISABLE строку каталога не убирает. Наличия имени мало.
const triggersSQL = `SELECT t.tgname, t.tgenabled = 'A', p.proname
FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
WHERE t.tgrelid = $1 AND NOT t.tgisinternal`

// CheckSchema сверяет таблицы inbox_* с миграциями, ничего не меняя: колонки и
// типы, ключи, CHECK и каскад внешнего ключа, индексы, триггеры с режимом ENABLE
// ALWAYS и их функцию. Зовётся на старте потребителя; все расхождения — в одной
// ошибке, по именам. Сбой запроса к каталогу — inbox.ErrUnavailable.
//
// Лишние колонки и индексы потребителя расхождением не считаются.
func (s *Store) CheckSchema(ctx context.Context) error {
	var problems []error
	for _, table := range slices.Sorted(maps.Keys(expected)) {
		found, err := s.checkTable(ctx, table)
		if err != nil {
			return err
		}
		problems = append(problems, found...)
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.Join(append([]error{errors.New(mismatchHint)}, problems...)...)
}

func (s *Store) checkTable(ctx context.Context, table string) ([]error, error) {
	var (
		oid    uint32
		schema string
	)
	err := s.db().QueryRow(ctx, tableSQL, table).Scan(&oid, &schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return []error{fmt.Errorf(missingTableHint, table)}, nil
	}
	if err != nil {
		return nil, storeError("check schema", err)
	}

	spec := expected[table]
	var problems []error
	for _, check := range []func() ([]error, error){
		func() ([]error, error) { return s.checkColumns(ctx, table, schema, spec) },
		func() ([]error, error) { return s.checkConstraints(ctx, table, oid, spec) },
		func() ([]error, error) { return s.checkIndexes(ctx, table, schema, spec) },
		func() ([]error, error) { return s.checkTriggers(ctx, table, oid, spec) },
	} {
		found, checkErr := check()
		if checkErr != nil {
			return nil, checkErr
		}
		problems = append(problems, found...)
	}
	return problems, nil
}

func (s *Store) checkColumns(ctx context.Context, table, schema string, spec tableSpec) ([]error, error) {
	actual, err := queryPairs[string](ctx, s.db(), columnsSQL, schema, table)
	if err != nil {
		return nil, storeError("check schema: columns", err)
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(spec.columns)) {
		got, ok := actual[name]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: колонки %s нет", table, name))
			continue
		}
		if got != spec.columns[name] {
			problems = append(problems, fmt.Errorf("%s: колонка %s имеет тип %s, ожидается %s",
				table, name, got, spec.columns[name]))
		}
	}
	return problems, nil
}

func (s *Store) checkConstraints(ctx context.Context, table string, oid uint32, spec tableSpec) ([]error, error) {
	actual, err := queryPairs[bool](ctx, s.db(), constraintsSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	var problems []error
	for _, name := range spec.constraints {
		formOK, ok := actual[name]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: ограничения %s нет", table, name))
			continue
		}
		if !formOK {
			problems = append(problems, fmt.Errorf("%s: внешний ключ %s не ON DELETE CASCADE — тело переживёт отметку",
				table, name))
		}
	}
	return problems, nil
}

func (s *Store) checkIndexes(ctx context.Context, table, schema string, spec tableSpec) ([]error, error) {
	rows, err := s.db().Query(ctx, indexesSQL, schema, table)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	actual, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []error
	for _, name := range spec.indexes {
		if !slices.Contains(actual, name) {
			problems = append(problems, fmt.Errorf("%s: индекса %s нет", table, name))
		}
	}
	return problems, nil
}

// catalogTrigger — триггер из каталога вместе с его функцией.
type catalogTrigger struct {
	always   bool
	function string
}

func (s *Store) checkTriggers(ctx context.Context, table string, oid uint32, spec tableSpec) ([]error, error) {
	rows, err := s.db().Query(ctx, triggersSQL, oid)
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	actual := map[string]catalogTrigger{}
	var (
		name string
		row  catalogTrigger
	)
	_, err = pgx.ForEachRow(rows, []any{&name, &row.always, &row.function}, func() error {
		actual[name] = row
		return nil
	})
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	var problems []error
	for _, trigger := range slices.Sorted(maps.Keys(spec.triggers)) {
		got, ok := actual[trigger]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: триггера %s нет", table, trigger))
			continue
		}
		if !got.always {
			problems = append(problems, fmt.Errorf("%s: триггер %s не ENABLE ALWAYS — он молчит при репликации", table, trigger))
		}
		if want := spec.triggers[trigger]; got.function != want {
			problems = append(problems, fmt.Errorf("%s: триггер %s зовёт функцию %s, ожидается %s",
				table, trigger, got.function, want))
		}
	}
	return problems, nil
}

// queryPairs — строки «имя, значение» в карту.
func queryPairs[V any](ctx context.Context, q postgres.Querier, query string, args ...any) (map[string]V, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]V{}
	var (
		name  string
		value V
	)
	_, err = pgx.ForEachRow(rows, []any{&name, &value}, func() error {
		out[name] = value
		return nil
	})
	return out, err
}
