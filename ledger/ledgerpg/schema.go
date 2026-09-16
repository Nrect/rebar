package ledgerpg

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/postgres"
)

// Имена таблиц. Без имени схемы: search_path выбирает потребитель.
const (
	tableBooks    = "ledger_books"
	tableKinds    = "ledger_kinds"
	tableAccounts = "ledger_accounts"
	tableEntries  = "ledger_entries"
)

// Имена, которыми база отказывает записи, — контракт наравне с портом: адаптер
// разбирает отказ по ним (refusals), и переименование в миграции ломает разбор
// молча. Триггер представляется именем через RAISE … USING CONSTRAINT.
const (
	fkAccountsBook              = "ledger_accounts_book_fkey"
	fkEntriesAccount            = "ledger_entries_account_fkey"
	fkEntriesKind               = "ledger_entries_kind_fkey"
	uxEntriesReversal           = "ux_ledger_entries_reversal"
	ckEntriesAmount             = "ledger_entries_amount_chk"
	ckEntriesKey                = "ledger_entries_key_chk"
	ckEntriesKeyID              = "ledger_entries_key_id_chk"
	ckEntriesHash               = "ledger_entries_hash_chk"
	ckEntriesReversal           = "ledger_entries_reversal_chk"
	ckEntriesSign               = "ledger_entries_sign"
	ckEntriesRequired           = "ledger_entries_required"
	ckEntriesReversalTarget     = "ledger_entries_reversal_target"
	ckEntriesReversalOfReversal = "ledger_entries_reversal_of_reversal"
	ckEntriesReversalAmount     = "ledger_entries_reversal_amount"
	ckEntriesBalanceRange       = "ledger_entries_balance_range"
	ckEntriesChain              = "ledger_entries_chain"
	ckEntriesHeadMoved          = "ledger_entries_head_moved"
	ckEntriesTaken              = "ledger_entries_taken"
	ckEntriesFloor              = "ledger_entries_floor"
)

// Функции, которые зовут по два триггера. Их имена — они же имена отказов
// (RAISE … USING CONSTRAINT): правку журнала и головы адаптер не шлёт.
const (
	fnAccountsGuard    = "ledger_accounts_guard"
	fnEntriesImmutable = "ledger_entries_immutable"
)

// Колонки, общие у нескольких таблиц.
const (
	colBook      = "book"
	colAccount   = "account"
	colSeq       = "seq"
	colKind      = "kind"
	colReference = "reference"
)

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeUUID        = "uuid"
	typeBigint      = "bigint"
	typeBytea       = "bytea"
	typeTimestamptz = "timestamp with time zone"
)

// tableSpec — что обязано быть у таблицы. Меняется только вместе с миграцией;
// страж расхождения — TestExpectedSchema_MatchesMigrations.
type tableSpec struct {
	columns map[string]string
	// constraints — CHECK и внешние ключи по имени.
	constraints []string
	// indexes — имя → обязан ли быть уникальным.
	indexes  map[string]bool
	triggers map[string]triggerSpec
}

// triggerSpec — функция триггера и её права.
type triggerSpec struct {
	function string
	// definer — функция пишет то, на что у роли приложения прав нет.
	definer bool
}

var expected = map[string]tableSpec{
	tableBooks: {
		columns:     map[string]string{colBook: typeText, "unit": typeText, "floor_minor": typeBigint},
		constraints: []string{"ledger_books_book_chk", "ledger_books_floor_chk", "ledger_books_unit_chk"},
		indexes:     map[string]bool{"ledger_books_pkey": true},
	},
	tableKinds: {
		columns: map[string]string{
			colBook: typeText, colKind: typeText, "sign": typeText, colReference: typeText, "attribution": typeText,
		},
		constraints: []string{
			"ledger_kinds_attribution_chk", "ledger_kinds_book_fkey", "ledger_kinds_kind_chk",
			"ledger_kinds_reference_chk", "ledger_kinds_reversal_chk", "ledger_kinds_sign_chk",
		},
		indexes: map[string]bool{"ledger_kinds_pkey": true},
	},
	tableAccounts: {
		columns: map[string]string{
			colBook: typeText, colAccount: typeUUID, colSeq: typeBigint, "balance_minor": typeBigint,
			"last_hash": typeBytea, "version": typeBigint,
		},
		constraints: []string{fkAccountsBook, "ledger_accounts_head_chk"},
		indexes:     map[string]bool{"ledger_accounts_pkey": true},
		triggers: map[string]triggerSpec{
			"ledger_accounts_guard_trg":       {function: fnAccountsGuard},
			"ledger_accounts_no_truncate_trg": {function: fnAccountsGuard},
		},
	},
	tableEntries: {
		columns: map[string]string{
			"id": typeUUID, colBook: typeText, colAccount: typeUUID, colSeq: typeBigint, colKind: typeText,
			"amount_minor": typeBigint, "balance_after_minor": typeBigint, colReference: typeText,
			"reverses_id": typeUUID, "reason": typeText, "actor": typeText, "idempotency_key": typeText,
			"created_at": typeTimestamptz, "key_id": "integer", "prev_hash": typeBytea, "entry_hash": typeBytea,
		},
		constraints: []string{
			ckEntriesAmount, ckEntriesHash, ckEntriesKey, ckEntriesKeyID, ckEntriesReversal,
			fkEntriesAccount, fkEntriesKind,
		},
		indexes: map[string]bool{
			"ledger_entries_pkey": true, "ux_ledger_entries_key": true, "ux_ledger_entries_seq": true,
			uxEntriesReversal: true,
		},
		triggers: map[string]triggerSpec{
			"ledger_entries_apply_trg":       {function: "ledger_entries_apply", definer: true},
			"ledger_entries_check_trg":       {function: "ledger_entries_check"},
			"ledger_entries_immutable_trg":   {function: fnEntriesImmutable},
			"ledger_entries_no_truncate_trg": {function: fnEntriesImmutable},
		},
	},
}

// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
const (
	missingTableHint = "ledgerpg.CheckSchema: таблицы %s нет: накатите ledgerpg.Migrations() раннером проекта"
	mismatchHint     = "ledgerpg.CheckSchema: схема расходится с миграциями — накатите ledgerpg.Migrations() раннером проекта; что осталось после наката, чините своей миграцией"
)

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass($1)`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

const constraintsSQL = `SELECT conname FROM pg_constraint WHERE conrelid = $1 AND contype IN ('c', 'f')`

const indexesSQL = `SELECT indexname, indexdef LIKE 'CREATE UNIQUE INDEX %'
FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

// tgenabled = 'A' — ENABLE ALWAYS: в режиме по умолчанию ('O') триггер молчит
// при репликации, то есть там, где журнал и правят руками. Наличия имени мало.
const triggersSQL = `SELECT t.tgname, t.tgenabled = 'A', p.proname, p.prosecdef, COALESCE(p.proconfig, '{}')
FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
WHERE t.tgrelid = $1 AND NOT t.tgisinternal`

const (
	bookSQL  = `SELECT unit, floor_minor FROM ledger_books WHERE book = $1`
	kindsSQL = `SELECT kind, sign, reference, attribution FROM ledger_kinds WHERE book = $1`
)

// CheckSchema сверяет таблицы ledger_* с миграциями, а справочники книг и родов —
// с книгами из New, ничего не меняя: колонки и типы, CHECK и внешние ключи,
// индексы, режим ENABLE ALWAYS у триггеров, права и search_path их функций.
// Зовётся на старте потребителя; все расхождения — в одной ошибке, по именам.
//
// Лишние колонки и индексы потребителя расхождением не считаются. Лишний род в
// справочнике — считается: база приняла бы движение, которого нет в реестре.
func (s *Store) CheckSchema(ctx context.Context) error {
	var problems []error
	for _, table := range slices.Sorted(maps.Keys(expected)) {
		found, err := s.checkTable(ctx, table)
		if err != nil {
			return err
		}
		problems = append(problems, found...)
	}
	// Справочник читается, только когда таблицы на месте: иначе запрос к нему
	// упал бы сбоем и спрятал бы расхождения схемы.
	if len(problems) == 0 {
		for _, name := range slices.Sorted(maps.Keys(s.books)) {
			found, err := s.checkBook(ctx, name, s.books[name])
			if err != nil {
				return err
			}
			problems = append(problems, found...)
		}
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
	rows, err := s.db().Query(ctx, constraintsSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	actual, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	var problems []error
	for _, name := range spec.constraints {
		if !slices.Contains(actual, name) {
			problems = append(problems, fmt.Errorf("%s: ограничения %s нет", table, name))
		}
	}
	return problems, nil
}

func (s *Store) checkIndexes(ctx context.Context, table, schema string, spec tableSpec) ([]error, error) {
	actual, err := queryPairs[bool](ctx, s.db(), indexesSQL, schema, table)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(spec.indexes)) {
		unique, ok := actual[name]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: индекса %s нет", table, name))
			continue
		}
		if spec.indexes[name] && !unique {
			problems = append(problems, fmt.Errorf("%s: индекс %s не уникальный", table, name))
		}
	}
	return problems, nil
}

// catalogTrigger — триггер из каталога вместе с его функцией.
type catalogTrigger struct {
	always   bool
	function string
	definer  bool
	config   []string
}

func (s *Store) checkTriggers(ctx context.Context, table string, oid uint32, spec tableSpec) ([]error, error) {
	if len(spec.triggers) == 0 {
		return nil, nil
	}
	rows, err := s.db().Query(ctx, triggersSQL, oid)
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	actual := map[string]catalogTrigger{}
	var (
		name string
		row  catalogTrigger
	)
	_, err = pgx.ForEachRow(rows, []any{&name, &row.always, &row.function, &row.definer, &row.config}, func() error {
		row.config = slices.Clone(row.config)
		actual[name] = row
		return nil
	})
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	var problems []error
	checked := map[string]bool{}
	for _, trigger := range slices.Sorted(maps.Keys(spec.triggers)) {
		problems = append(problems, triggerProblems(table, trigger, spec.triggers[trigger], actual, checked)...)
	}
	return problems, nil
}

// triggerProblems — расхождения одного триггера: он есть и ENABLE ALWAYS, зовёт
// свою функцию, у функции нужные права и закреплённый search_path. Функцию двух
// триггеров checked проверяет один раз: одно расхождение — одна строка.
func triggerProblems(table, name string, want triggerSpec, actual map[string]catalogTrigger, checked map[string]bool,
) []error {
	got, ok := actual[name]
	if !ok {
		return []error{fmt.Errorf("%s: триггера %s нет", table, name)}
	}
	var problems []error
	if !got.always {
		problems = append(problems, fmt.Errorf("%s: триггер %s не ENABLE ALWAYS — он молчит при репликации", table, name))
	}
	if got.function != want.function {
		return append(problems, fmt.Errorf("%s: триггер %s зовёт функцию %s, ожидается %s",
			table, name, got.function, want.function))
	}
	if checked[got.function] {
		return problems
	}
	checked[got.function] = true
	if want.definer && !got.definer {
		problems = append(problems, fmt.Errorf(
			"%s: функция %s не SECURITY DEFINER — роль приложения не запишет остаток", table, got.function))
	}
	if !pinsSearchPath(got.config) {
		problems = append(problems, fmt.Errorf(
			"%s: у функции %s search_path не закреплён с pg_temp последней", table, got.function))
	}
	return problems
}

// pinsSearchPath — у функции свой search_path, и pg_temp в нём последняя: иначе
// временная таблица с именем справочника подменила бы его внутри триггера.
func pinsSearchPath(config []string) bool {
	for _, setting := range config {
		value, ok := strings.CutPrefix(setting, "search_path=")
		if !ok {
			continue
		}
		schemas := strings.Split(value, ",")
		return strings.TrimSpace(schemas[len(schemas)-1]) == "pg_temp"
	}
	return false
}

// checkBook — справочник книги равен реестру: строка книги с единицей и границей
// и роды с их знаком и обязательными полями, без лишних.
func (s *Store) checkBook(ctx context.Context, name string, want bookSpec) ([]error, error) {
	var (
		unit  string
		floor int64
	)
	err := s.db().QueryRow(ctx, bookSQL, name).Scan(&unit, &floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return []error{fmt.Errorf("%s: книги %s нет — справочник пишет миграция потребителя", tableBooks, name)}, nil
	}
	if err != nil {
		return nil, storeError("check schema: book", err)
	}
	var problems []error
	if unit != want.unit {
		problems = append(problems, fmt.Errorf("%s: у книги %s единица %s, в реестре %s", tableBooks, name, unit, want.unit))
	}
	if floor != want.floor {
		problems = append(problems, fmt.Errorf("%s: у книги %s граница %d, в реестре %d", tableBooks, name, floor, want.floor))
	}
	kinds, err := s.kindsOf(ctx, name)
	if err != nil {
		return nil, err
	}
	return append(problems, kindProblems(name, want.kinds, kinds)...), nil
}

// kindRow — род так, как его хранит справочник.
type kindRow struct {
	sign, reference, attribution string
}

func (r kindRow) String() string {
	return fmt.Sprintf("знак %s, основание %s, причина и автор %s", r.sign, r.reference, r.attribution)
}

func (s *Store) kindsOf(ctx context.Context, book string) (map[string]kindRow, error) {
	rows, err := s.db().Query(ctx, kindsSQL, book)
	if err != nil {
		return nil, storeError("check schema: kinds", err)
	}
	kinds := map[string]kindRow{}
	var (
		name string
		row  kindRow
	)
	_, err = pgx.ForEachRow(rows, []any{&name, &row.sign, &row.reference, &row.attribution}, func() error {
		kinds[name] = row
		return nil
	})
	if err != nil {
		return nil, storeError("check schema: kinds", err)
	}
	return kinds, nil
}

func kindProblems(book string, want []ledger.KindSpec, actual map[string]kindRow) []error {
	var problems []error
	declared := make(map[string]bool, len(want))
	for _, spec := range want {
		declared[spec.Name] = true
		wantRow := kindRow{sign: string(spec.Sign), reference: string(spec.Reference), attribution: string(spec.Attribution)}
		got, ok := actual[spec.Name]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: рода %s/%s нет", tableKinds, book, spec.Name))
			continue
		}
		if got != wantRow {
			problems = append(problems, fmt.Errorf("%s: у рода %s/%s в справочнике %s, в реестре %s",
				tableKinds, book, spec.Name, got, wantRow))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(actual)) {
		if !declared[name] {
			problems = append(problems, fmt.Errorf("%s: рода %s/%s нет в реестре книги", tableKinds, book, name))
		}
	}
	return problems
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
