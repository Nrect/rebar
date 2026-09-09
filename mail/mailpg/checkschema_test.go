package mailpg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
)

// Schema — то, что потребитель применит из кода: обязан совпадать с файлом,
// который он же может скопировать в миграции.
func TestSchema_EmbedEqualsFile(t *testing.T) {
	t.Parallel()
	assert.Equal(t, readSchema(t), mailpg.Schema)
}

func TestCheckSchema_FullSchemaPasses(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	require.NoError(t, store.CheckSchema(context.Background()))
}

// Первая строка ошибки — что делать: скопировать schema.sql в миграции.
func TestCheckSchema_MissingTableTellsWhatToDo(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	err := mailpg.New(pool).CheckSchema(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "таблицы email_outbox нет")
	assert.Contains(t, err.Error(), "schema.sql")
	assert.Contains(t, err.Error(), "Миграция")
	assert.NotErrorIs(t, err, mail.ErrUnavailable, "расхождение схемы — не временный сбой")
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
			name: "нет уникального индекса дедупа",
			ddl:  []string{`DROP INDEX ux_email_outbox_dedup`},
			want: []string{"индекса ux_email_outbox_dedup нет"},
		},
		{
			name: "индекс дедупа не уникальный",
			ddl: []string{
				`DROP INDEX ux_email_outbox_dedup`,
				`CREATE INDEX ux_email_outbox_dedup ON email_outbox (dedup_key)`,
			},
			want: []string{"индекс ux_email_outbox_dedup не уникальный"},
		},
		{
			name: "нет CHECK",
			ddl:  []string{`ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_lock_chk`},
			want: []string{"ограничения email_outbox_lock_chk нет"},
		},
		{
			// Схема, скопированная до v0.2.0, держит тот же CHECK под именем от
			// Postgres: CheckSchema обязан сказать об этом на старте, а не
			// оставить потребителя с ограничением, которого код не знает.
			name: "словарь статусов под чужим именем",
			ddl:  []string{`ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_status_chk`},
			want: []string{"ограничения email_outbox_status_chk нет"},
		},
		{
			name: "нет колонки и другой тип",
			ddl: []string{
				`ALTER TABLE email_outbox DROP COLUMN provider_message_id`,
				`ALTER TABLE email_outbox ALTER COLUMN fingerprint TYPE TEXT USING encode(fingerprint, 'hex')`,
			},
			want: []string{
				"колонки provider_message_id нет",
				"колонка fingerprint: тип text, ожидается bytea",
			},
		},
		{
			name: "несколько расхождений сразу",
			ddl: []string{
				`DROP INDEX ix_email_outbox_due`,
				`ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_body_cleared_chk`,
			},
			want: []string{
				"индекса ix_email_outbox_due нет",
				"ограничения email_outbox_body_cleared_chk нет",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			mustEnqueue(t, store, envelope()) // в таблице есть строка с телом письма
			for _, ddl := range tt.ddl {
				_, err := pool.Exec(context.Background(), ddl)
				require.NoError(t, err, ddl)
			}

			err := store.CheckSchema(context.Background())

			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			assert.Contains(t, lines[0], "расходится с mailpg/schema.sql", "первая строка — что делать")
			for _, want := range tt.want {
				assert.Contains(t, lines[1:], want)
			}
			assert.Len(t, lines, 1+len(tt.want), "лишних расхождений нет")
			assert.NotContains(t, err.Error(), secretLink)
			assert.NotErrorIs(t, err, mail.ErrUnavailable)
		})
	}
}

// Потребитель вправе расширять таблицу своими колонками и индексами.
func TestCheckSchema_IgnoresConsumerAdditions(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	for _, ddl := range []string{
		`ALTER TABLE email_outbox ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX ix_email_outbox_tenant ON email_outbox (tenant_id)`,
	} {
		_, err := pool.Exec(context.Background(), ddl)
		require.NoError(t, err, ddl)
	}

	require.NoError(t, store.CheckSchema(context.Background()))
}

// Сбой каталога — временный: mail.ErrUnavailable, как у любого запроса адаптера.
func TestCheckSchema_CatalogFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	pool.Close()

	err := store.CheckSchema(context.Background())

	require.ErrorIs(t, err, mail.ErrUnavailable)
}
