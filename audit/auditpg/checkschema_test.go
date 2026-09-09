package auditpg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

func TestCheckSchema_FullSchemaPasses(t *testing.T) {
	t.Parallel()
	sink, _ := newSink(t)

	require.NoError(t, sink.CheckSchema(context.Background()))
}

// Первая строка ошибки — что делать: скопировать schema.sql в миграции.
func TestCheckSchema_MissingTableTellsWhatToDo(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	err := auditpg.New(pool).CheckSchema(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "таблицы audit_events нет")
	assert.Contains(t, err.Error(), "schema.sql")
	assert.NotErrorIs(t, err, audit.ErrUnavailable, "расхождение схемы — не временный сбой")
}

// Расхождения собираются в одну ошибку и называют объект по имени; строки
// таблицы в текст не попадают.
func TestCheckSchema_ReportsEveryMismatchByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ddl  []string
		want []string
	}{
		{
			name: "нет индекса по времени",
			ddl:  []string{`DROP INDEX ix_audit_events_occurred_at`},
			want: []string{"индекса ix_audit_events_occurred_at нет"},
		},
		{
			name: "нет индекса по актору и по цели",
			ddl:  []string{`DROP INDEX ix_audit_events_actor`, `DROP INDEX ix_audit_events_target`},
			want: []string{"индекса ix_audit_events_actor нет", "индекса ix_audit_events_target нет"},
		},
		{
			name: "нет CHECK закрытого набора",
			ddl:  []string{`ALTER TABLE audit_events DROP CONSTRAINT audit_events_outcome_chk`},
			want: []string{"ограничения audit_events_outcome_chk нет"},
		},
		{
			name: "нет колонки и другой тип",
			ddl: []string{
				`ALTER TABLE audit_events DROP COLUMN request_id`,
				`ALTER TABLE audit_events ALTER COLUMN details TYPE TEXT USING details::text`,
			},
			want: []string{
				"колонки request_id нет",
				"колонка details: тип text, ожидается jsonb",
			},
		},
		{
			name: "нет триггера неизменяемости",
			ddl:  []string{`DROP TRIGGER audit_events_append_only_trg ON audit_events`},
			want: []string{"триггера audit_events_append_only_trg нет: журнал перестал быть append-only"},
		},
		{
			// DISABLE не трогает ни одной строки каталога, которую заметил бы
			// поиск по имени: триггер на месте, а append-only уже нет.
			name: "триггер выключен",
			ddl:  []string{`ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only_trg`},
			want: []string{"триггер audit_events_append_only_trg не в режиме ENABLE ALWAYS: журнал правится при репликации и после DISABLE TRIGGER"},
		},
		{
			// Режим по умолчанию после восстановления дампа: триггер молчит
			// на реплике, то есть там, где журнал и правят руками.
			name: "триггер в режиме по умолчанию",
			ddl: []string{
				`ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only_trg`,
			},
			want: []string{"триггер audit_events_append_only_trg не в режиме ENABLE ALWAYS: журнал правится при репликации и после DISABLE TRIGGER"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink, pool := newSink(t)
			ev := testEvent()
			require.NoError(t, sink.Write(context.Background(), ev)) // в таблице есть строка с данными
			for _, ddl := range tt.ddl {
				_, err := pool.Exec(context.Background(), ddl)
				require.NoError(t, err, ddl)
			}

			err := sink.CheckSchema(context.Background())

			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			assert.Contains(t, lines[0], "расходится с auditpg/schema.sql", "первая строка — что делать")
			for _, want := range tt.want {
				assert.Contains(t, lines[1:], want)
			}
			assert.Len(t, lines, 1+len(tt.want), "лишних расхождений нет")
			assert.NotContains(t, err.Error(), secretName)
			assert.NotErrorIs(t, err, audit.ErrUnavailable)
		})
	}
}

// Потребитель вправе расширять таблицу своими колонками и индексами.
func TestCheckSchema_IgnoresConsumerAdditions(t *testing.T) {
	t.Parallel()
	sink, pool := newSink(t)
	for _, ddl := range []string{
		`ALTER TABLE audit_events ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX ix_audit_events_tenant ON audit_events (tenant_id)`,
	} {
		_, err := pool.Exec(context.Background(), ddl)
		require.NoError(t, err, ddl)
	}

	require.NoError(t, sink.CheckSchema(context.Background()))
}

// Сбой каталога — временный: audit.ErrUnavailable, как у любого запроса адаптера.
func TestCheckSchema_CatalogFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	sink, pool := newSink(t)
	pool.Close()

	err := sink.CheckSchema(context.Background())

	require.ErrorIs(t, err, audit.ErrUnavailable)
}

// Обе стороны миграции применяются на пустую базу: Down обязан снимать и
// таблицу, и функцию триггера, иначе повторный накат упадёт.
func TestSchema_UpAndDownApplyToEmptyDatabase(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)
	ctx := context.Background()

	up := pgtest.GooseUp(t, schemaPath)
	down := gooseDown(t)

	for range 2 {
		pgtest.Apply(t, pool, up)
		require.NoError(t, auditpg.New(pool).CheckSchema(ctx))
		pgtest.Apply(t, pool, down)
	}
}
