package paymentpg

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

// Имена таблиц. Без имени схемы: search_path выбирает потребитель.
const (
	tableIntents = "payment_intents"
	tableItems   = "payment_intent_items"
	tableEvents  = "payment_events"
	tableLedger  = "payment_ledger"
)

// Имена ограничений, НА КОТОРЫЕ ССЫЛАЕТСЯ КОД, — часть публичного контракта:
// переименование в миграции потребителя ломает разбор конфликта молча.
const (
	// uxIntentsKey — арбитр ON CONFLICT при создании намерения: занятый ключ
	// идемпотентности это повтор.
	uxIntentsKey = "ux_payment_intents_key"
	// uxIntentsLiveReference — «одно живое намерение на ссылку»: занятая
	// ссылка это отказ (ErrReferenceBusy), а не повтор.
	uxIntentsLiveReference = "ux_payment_intents_live_reference"
	// uxEventsDedup — арбитр ON CONFLICT приёма событий провайдера.
	uxEventsDedup = "ux_payment_events_dedup"
	// uxLedgerCapture — одно зачисление на намерение.
	uxLedgerCapture = "ux_payment_ledger_capture"
	// uxLedgerKey — идемпотентность строки книги в пределах намерения.
	uxLedgerKey = "ux_payment_ledger_key"

	// Имена, которыми представляются триггеры книги (RAISE … USING CONSTRAINT):
	// нарушение отличимо по имени так же, как нарушение индекса.
	ckLedgerImmutable      = "payment_ledger_immutable"
	ckLedgerRefundCap      = "payment_ledger_refund_cap"
	ckLedgerRefundCurrency = "payment_ledger_refund_currency"
)

// Имена колонок, встречающихся в нескольких таблицах.
const (
	colIntentID    = "intent_id"
	colAmountMinor = "amount_minor"
	colCurrency    = "currency"
)

// Типы из information_schema.columns.data_type.
const (
	typeText        = "text"
	typeUUID        = "uuid"
	typeBigint      = "bigint"
	typeInt         = "integer"
	typeTimestamptz = "timestamp with time zone"
)

// tableSpec — что обязано быть у таблицы. Меняется только вместе со schema.sql;
// страж расхождения — TestExpectedSchema_MatchesFile.
type tableSpec struct {
	columns  map[string]string
	checks   []string
	indexes  map[string]bool // имя → обязан ли быть уникальным
	triggers []string        // обязаны быть ENABLE ALWAYS
}

var expected = map[string]tableSpec{
	tableIntents: {
		columns: map[string]string{
			"id": typeUUID, "payer_id": typeUUID, "reference": typeText,
			colAmountMinor: typeBigint, colCurrency: typeText, "provider": typeText,
			"method": typeText, "auto_capture": "boolean", "provider_payment_id": typeText,
			"confirmation_type": typeText, "confirmation_url": typeText, "confirmation_qr": typeText,
			"status": typeText, "idempotency_key": typeText, "params_fingerprint": "bytea",
			"created_at": typeTimestamptz, "updated_at": typeTimestamptz,
			"expires_at": typeTimestamptz, "settled_at": typeTimestamptz,
		},
		checks: []string{
			"payment_intents_amount_chk", "payment_intents_confirmation_chk",
			"payment_intents_currency_chk", "payment_intents_expires_chk",
			"payment_intents_fingerprint_chk", "payment_intents_method_chk",
			"payment_intents_provider_chk", "payment_intents_reference_chk",
			"payment_intents_settled_chk", "payment_intents_status_chk",
		},
		indexes: map[string]bool{
			uxIntentsKey: true, uxIntentsLiveReference: true, "ix_payment_intents_open": false,
		},
	},
	tableItems: {
		columns: map[string]string{
			colIntentID: typeUUID, "position": typeInt, "product_id": typeText,
			"title": typeText, colAmountMinor: typeBigint, "quantity": typeInt,
		},
		checks: []string{
			"payment_intent_items_amount_chk", "payment_intent_items_position_chk",
			"payment_intent_items_product_chk", "payment_intent_items_quantity_chk",
		},
		indexes: map[string]bool{"payment_intent_items_pkey": true},
	},
	tableEvents: {
		columns: map[string]string{
			"provider": typeText, "provider_event_id": typeText, colIntentID: typeUUID,
			"kind": typeText, colAmountMinor: typeBigint, colCurrency: typeText,
			"provider_payment_id": typeText, "occurred_at": typeTimestamptz,
			"received_at": typeTimestamptz, "deliveries": typeInt,
		},
		checks: []string{
			"payment_events_amount_chk", "payment_events_currency_chk",
			"payment_events_deliveries_chk", "payment_events_id_chk",
			"payment_events_kind_chk", "payment_events_provider_chk",
		},
		indexes: map[string]bool{
			uxEventsDedup: true, "ix_payment_events_intent": false, "ix_payment_events_orphan": false,
		},
	},
	tableLedger: {
		columns: map[string]string{
			"id": typeUUID, colIntentID: typeUUID, "kind": typeText, colAmountMinor: typeBigint,
			colCurrency: typeText, "provider_event_id": typeText, "reverses_entry_id": typeUUID,
			"idempotency_key": typeText, "actor_id": typeUUID, "created_at": typeTimestamptz,
		},
		checks: []string{
			"payment_ledger_amount_chk", "payment_ledger_currency_chk", "payment_ledger_key_chk",
			"payment_ledger_kind_chk", "payment_ledger_refund_chk",
		},
		indexes: map[string]bool{
			uxLedgerCapture: true, uxLedgerKey: true,
			"ix_payment_ledger_intent": false, "ix_payment_ledger_reverses": false,
		},
		triggers: []string{
			"payment_ledger_immutable_trg", "payment_ledger_no_truncate_trg",
			"payment_ledger_refund_cap_trg",
		},
	},
}

// Первая строка ошибки — что делать; расхождения перечисляются ниже неё.
const (
	missingTableHint = "paymentpg.CheckSchema: таблицы %s нет: скопируйте paymentpg/schema.sql в миграции"
	mismatchHint     = "paymentpg.CheckSchema: схема расходится с paymentpg/schema.sql — сверьте миграцию"
)

// to_regclass ищет таблицу по search_path соединения — там же, где её найдут
// запросы адаптера; дальше каталог опрашивается по oid и имени схемы.
const tableSQL = `SELECT c.oid, n.nspname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = to_regclass($1)`

const columnsSQL = `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2`

const checksSQL = `SELECT conname FROM pg_constraint WHERE conrelid = $1 AND contype = 'c'`

const indexesSQL = `SELECT indexname, indexdef LIKE 'CREATE UNIQUE INDEX %'
FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`

// tgenabled = 'A' — это ENABLE ALWAYS. Проверяется именно оно: триггер в
// режиме по умолчанию ('O') молчит при репликации, то есть ровно там, где
// книгу и правят в обход приложения.
const triggersSQL = `SELECT tgname, tgenabled = 'A' FROM pg_trigger
WHERE tgrelid = $1 AND NOT tgisinternal`

// CheckSchema сверяет таблицы payment_* со schema.sql, ничего не меняя:
// колонки и их типы, именованные CHECK, индексы и триггеры книги. Зовётся на
// старте потребителя: миграцию применяет он сам, пакет только проверяет.
//
// Автомиграции из библиотеки нет намеренно: две правды о схеме, DDL-права у
// приложения и гонка реплик при выкате. Лишние колонки потребителя
// расхождением не считаются. Все расхождения — в одной ошибке (errors.Join).
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
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return []error{fmt.Errorf(missingTableHint, table)}, nil
	case err != nil:
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

func (s *Store) checkColumns(ctx context.Context, table, schema string, spec tableSpec,
) ([]error, error) {
	actual, err := queryPairs[string](ctx, s.db(), columnsSQL, schema, table)
	if err != nil {
		return nil, storeError("check schema: columns", err)
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(spec.columns)) {
		switch got, ok := actual[name]; {
		case !ok:
			problems = append(problems, fmt.Errorf("%s: колонки %s нет", table, name))
		case got != spec.columns[name]:
			problems = append(problems, fmt.Errorf("%s: колонка %s имеет тип %s, ожидается %s",
				table, name, got, spec.columns[name]))
		}
	}
	return problems, nil
}

func (s *Store) checkConstraints(ctx context.Context, table string, oid uint32, spec tableSpec,
) ([]error, error) {
	rows, err := s.db().Query(ctx, checksSQL, oid)
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	actual, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, storeError("check schema: constraints", err)
	}
	var problems []error
	for _, name := range spec.checks {
		if !slices.Contains(actual, name) {
			problems = append(problems, fmt.Errorf("%s: ограничения %s нет", table, name))
		}
	}
	return problems, nil
}

func (s *Store) checkIndexes(ctx context.Context, table, schema string, spec tableSpec,
) ([]error, error) {
	actual, err := queryPairs[bool](ctx, s.db(), indexesSQL, schema, table)
	if err != nil {
		return nil, storeError("check schema: indexes", err)
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(spec.indexes)) {
		switch unique, ok := actual[name]; {
		case !ok:
			problems = append(problems, fmt.Errorf("%s: индекса %s нет", table, name))
		case spec.indexes[name] && !unique:
			problems = append(problems, fmt.Errorf("%s: индекс %s не уникальный", table, name))
		}
	}
	return problems, nil
}

func (s *Store) checkTriggers(ctx context.Context, table string, oid uint32, spec tableSpec,
) ([]error, error) {
	if len(spec.triggers) == 0 {
		return nil, nil
	}
	actual, err := queryPairs[bool](ctx, s.db(), triggersSQL, oid)
	if err != nil {
		return nil, storeError("check schema: triggers", err)
	}
	var problems []error
	for _, name := range spec.triggers {
		switch always, ok := actual[name]; {
		case !ok:
			problems = append(problems, fmt.Errorf("%s: триггера %s нет", table, name))
		case !always:
			problems = append(problems, fmt.Errorf(
				"%s: триггер %s не ENABLE ALWAYS — он молчит при репликации", table, name))
		}
	}
	return problems, nil
}

// queryPairs — строки «имя, значение» в карту.
func queryPairs[V any](ctx context.Context, q querier, sql string, args ...any) (map[string]V, error) {
	rows, err := q.Query(ctx, sql, args...)
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
