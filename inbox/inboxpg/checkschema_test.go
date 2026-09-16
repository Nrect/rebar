package inboxpg_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
)

// mismatchLine — первая строка расхождения: что делать.
const mismatchLine = "inboxpg.CheckSchema: схема расходится с миграциями — накатите inboxpg.Migrations() " +
	"раннером проекта; что осталось после наката, чините своей миграцией"

// CheckSchema сверяет, но не применяет: на пустой схеме он называет каждую
// таблицу и говорит, что делать, а таблиц не заводит.
func TestCheckSchema_MissingTablesTellWhatToDo(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	err := inboxpg.New(pool, only(nop)).CheckSchema(t.Context())
	require.Error(t, err)
	for _, table := range []string{"inbox_events", "inbox_payloads"} {
		assert.Contains(t, err.Error(), "inboxpg.CheckSchema: таблицы "+table+" нет: накатите inboxpg.Migrations() раннером проекта")
	}
	require.NotErrorIs(t, err, inbox.ErrUnavailable, "расхождение схемы — не временный сбой")
	assert.Empty(t, schemaObjects(t, pool), "CheckSchema ничего не применяет")
}

// Каждое расхождение — строкой с именем объекта, первая строка — что делать.
// Лишних строк нет: расхождение не маскирует соседнее и не множится.
func TestCheckSchema_ReportsEveryMismatchByName(t *testing.T) {
	t.Parallel()

	const noAlways = " не ENABLE ALWAYS — он молчит при репликации"
	tests := []struct {
		name string
		ddl  []string
		want []string
	}{
		{
			name: "нет индексов уборки",
			ddl:  []string{`DROP INDEX ix_inbox_events_received`, `DROP INDEX ix_inbox_payloads_received`},
			want: []string{
				"inbox_events: индекса ix_inbox_events_received нет",
				"inbox_payloads: индекса ix_inbox_payloads_received нет",
			},
		},
		{
			name: "нет CHECK отпечатка и потолка тела",
			ddl: []string{
				`ALTER TABLE inbox_events DROP CONSTRAINT inbox_events_digest_chk`,
				`ALTER TABLE inbox_payloads DROP CONSTRAINT inbox_payloads_size_chk`,
			},
			want: []string{
				"inbox_events: ограничения inbox_events_digest_chk нет",
				"inbox_payloads: ограничения inbox_payloads_size_chk нет",
			},
		},
		{
			// ON CONFLICT ON CONSTRAINT уникальный индекс того же имени не находит.
			name: "ключ дедупа — индекс, а не ограничение",
			ddl: []string{
				`ALTER TABLE inbox_payloads DROP CONSTRAINT inbox_payloads_event_fkey`,
				`ALTER TABLE inbox_events DROP CONSTRAINT ux_inbox_events_dedup`,
				`CREATE UNIQUE INDEX ux_inbox_events_dedup ON inbox_events (source, event_id)`,
			},
			want: []string{
				"inbox_events: ограничения ux_inbox_events_dedup нет",
				"inbox_payloads: ограничения inbox_payloads_event_fkey нет",
			},
		},
		{
			name: "внешний ключ без каскада",
			ddl: []string{
				`ALTER TABLE inbox_payloads DROP CONSTRAINT inbox_payloads_event_fkey`,
				`ALTER TABLE inbox_payloads ADD CONSTRAINT inbox_payloads_event_fkey
					FOREIGN KEY (source, event_id) REFERENCES inbox_events (source, event_id)`,
			},
			want: []string{"inbox_payloads: внешний ключ inbox_payloads_event_fkey не ON DELETE CASCADE — тело переживёт отметку"},
		},
		{
			name: "нет колонки и другой тип",
			ddl: []string{
				`ALTER TABLE inbox_events DROP COLUMN occurred_at`,
				`ALTER TABLE inbox_events ALTER COLUMN event_type TYPE VARCHAR(64)`,
			},
			want: []string{
				"inbox_events: колонки occurred_at нет",
				// CHECK момента зависит от колонки и уходит вместе с ней.
				"inbox_events: ограничения inbox_events_occurred_chk нет",
				"inbox_events: колонка event_type имеет тип character varying, ожидается text",
			},
		},
		{
			name: "триггер выключен",
			ddl:  []string{`ALTER TABLE inbox_events DISABLE TRIGGER inbox_events_append_only_trg`},
			want: []string{"inbox_events: триггер inbox_events_append_only_trg" + noAlways},
		},
		{
			// Режим по умолчанию: триггер на месте и молчит на реплике.
			name: "триггер в режиме ORIGIN",
			ddl:  []string{`ALTER TABLE inbox_payloads ENABLE TRIGGER inbox_payloads_append_only_trg`},
			want: []string{"inbox_payloads: триггер inbox_payloads_append_only_trg" + noAlways},
		},
		{
			name: "триггер в режиме REPLICA",
			ddl:  []string{`ALTER TABLE inbox_events ENABLE REPLICA TRIGGER inbox_events_append_only_trg`},
			want: []string{"inbox_events: триггер inbox_events_append_only_trg" + noAlways},
		},
		{
			// DROP + CREATE без явного ALTER приходит в ORIGIN (ADR-0011, решение 4).
			name: "триггер пересоздан без ENABLE ALWAYS",
			ddl: []string{
				`DROP TRIGGER inbox_payloads_append_only_trg ON inbox_payloads`,
				`CREATE TRIGGER inbox_payloads_append_only_trg BEFORE UPDATE ON inbox_payloads
					FOR EACH ROW EXECUTE FUNCTION inbox_append_only()`,
			},
			want: []string{"inbox_payloads: триггер inbox_payloads_append_only_trg" + noAlways},
		},
		{
			name: "нет триггера",
			ddl:  []string{`DROP TRIGGER inbox_events_append_only_trg ON inbox_events`},
			want: []string{"inbox_events: триггера inbox_events_append_only_trg нет"},
		},
		{
			name: "триггер зовёт чужую функцию",
			ddl: []string{
				`CREATE FUNCTION shop_noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
				`DROP TRIGGER inbox_events_append_only_trg ON inbox_events`,
				`CREATE TRIGGER inbox_events_append_only_trg BEFORE UPDATE ON inbox_events
					FOR EACH ROW EXECUTE FUNCTION shop_noop()`,
				`ALTER TABLE inbox_events ENABLE ALWAYS TRIGGER inbox_events_append_only_trg`,
			},
			want: []string{"inbox_events: триггер inbox_events_append_only_trg зовёт функцию shop_noop, ожидается inbox_append_only"},
		},
		{
			name: "нет таблицы тел",
			ddl:  []string{`DROP TABLE inbox_payloads`},
			want: []string{"inboxpg.CheckSchema: таблицы inbox_payloads нет: накатите inboxpg.Migrations() раннером проекта"},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t, only(nop))
			// В таблицах есть строки. Ключ свой у каждого подтеста: блокировка
			// ключа общая на базу, а не на схему.
			mustAccept(t, store, testEvent(fmt.Sprintf("evt-SECRET-row-%d", i), "SECRET-row"), inbox.OutcomeAccepted)
			for _, ddl := range tt.ddl {
				_, err := pool.Exec(t.Context(), ddl)
				require.NoError(t, err, ddl)
			}

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			assert.Equal(t, mismatchLine, lines[0], "первая строка — что делать")
			assert.ElementsMatch(t, tt.want, lines[1:], "расхождения по именам и без лишних")
			assert.NotContains(t, err.Error(), "SECRET", "данные строк в тексте")
			assert.NotErrorIs(t, err, inbox.ErrUnavailable)
		})
	}
}

// Полная схема проходит — и в транзакции потребителя тоже.
func TestCheckSchema_FullSchemaPasses(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	require.NoError(t, store.CheckSchema(t.Context()))
	require.NoError(t, store.WithTx(beginTx(t, pool)).CheckSchema(t.Context()))
}

// Потребитель вправе расширять таблицы своими колонками и индексами.
func TestCheckSchema_IgnoresConsumerAdditions(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	for _, ddl := range []string{
		`ALTER TABLE inbox_events ADD COLUMN tenant_id TEXT`,
		`CREATE INDEX ix_inbox_events_tenant ON inbox_events (tenant_id)`,
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err, ddl)
	}
	require.NoError(t, store.CheckSchema(t.Context()))
	mustAccept(t, store, testEvent("evt-extra-column", "extra"), inbox.OutcomeAccepted)
}

// Сбой каталога — временный: inbox.ErrUnavailable, как у любого запроса адаптера.
func TestCheckSchema_CatalogFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t, only(nop))
	pool.Close()
	require.ErrorIs(t, store.CheckSchema(t.Context()), inbox.ErrUnavailable)
}
