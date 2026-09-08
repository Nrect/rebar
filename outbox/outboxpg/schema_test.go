package outboxpg_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

const gooseDown = "-- +goose Down"

// Schema — то, что потребитель применит из кода: обязан совпадать с файлом,
// который он же может скопировать в миграции.
func TestSchema_EmbedEqualsFile(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	assert.Equal(t, string(raw), outboxpg.Schema)
}

// Схема — артефакт для goose потребителя: имена, на которые опирается код,
// проверяются в файле, а не в его копии.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()
	up, down, ok := strings.Cut(outboxpg.Schema, gooseDown)
	require.True(t, ok, "в schema.sql нет маркера %s", gooseDown)

	for _, want := range []string{
		"CREATE TABLE outbox_messages",
		"ux_outbox_messages_dedup", // имя индекса — часть контракта Store.Enqueue
		"ix_outbox_messages_due",
		"ix_outbox_messages_terminal",
		"ix_outbox_messages_failed",
		"ix_outbox_messages_aggregate",
		"outbox_messages_claim_chk",
		"outbox_messages_fail_chk",
	} {
		assert.Contains(t, up, want)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.Contains(t, down, "DROP TABLE outbox_messages")
	assert.NotContains(t, down, "CREATE TABLE")

	// Имя схемы не зашито и FK на таблицы потребителя нет: search_path
	// выбирает потребитель, а его таблицу пользователей пакет не знает.
	assert.NotContains(t, outboxpg.Schema, "public.")
	assert.NotContains(t, outboxpg.Schema, "REFERENCES")
	assert.NotContains(t, outboxpg.Schema, "DEFAULT now()",
		"время приходит параметром: иначе тесты на управляемых часах проверяют одно, а база пишет другое")
}

// Обе стороны миграции применяются на пустую базу: потребитель обязан уметь
// откатиться и накатить заново.
func TestSchemaFile_BothDirectionsApply(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	require.NoError(t, outboxpg.New(pool).CheckSchema(t.Context()))

	_, down, ok := strings.Cut(outboxpg.Schema, gooseDown)
	require.True(t, ok)
	pgtest.Apply(t, pool, down)
	require.Error(t, outboxpg.New(pool).CheckSchema(t.Context()), "после Down таблицы нет")

	pgtest.Apply(t, pool, pgtest.GooseUp(t, "schema.sql"))
	require.NoError(t, outboxpg.New(pool).CheckSchema(t.Context()))
}

func TestCheckSchema_FullSchemaPasses(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)

	require.NoError(t, store.CheckSchema(t.Context()))
}

// Первая строка ошибки — что делать: скопировать schema.sql в миграции.
func TestCheckSchema_MissingTableTellsWhatToDo(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	err := outboxpg.New(pool).CheckSchema(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "таблицы outbox_messages нет")
	assert.Contains(t, err.Error(), "schema.sql")
	assert.Contains(t, err.Error(), "Миграция")
	assert.NotErrorIs(t, err, outbox.ErrUnavailable, "расхождение схемы — не временный сбой")
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
			ddl:  []string{`DROP INDEX ux_outbox_messages_dedup`},
			want: []string{"индекса ux_outbox_messages_dedup нет"},
		},
		{
			name: "индекс дедупа перестал быть уникальным",
			ddl: []string{
				`DROP INDEX ux_outbox_messages_dedup`,
				`CREATE INDEX ux_outbox_messages_dedup ON outbox_messages (kind, dedup_key)`,
			},
			want: []string{"индекс ux_outbox_messages_dedup не уникальный"},
		},
		{
			name: "нет CHECK аренды",
			ddl:  []string{`ALTER TABLE outbox_messages DROP CONSTRAINT outbox_messages_claim_chk`},
			want: []string{"ограничения outbox_messages_claim_chk нет"},
		},
		{
			name: "нет колонки отпечатка",
			ddl:  []string{`ALTER TABLE outbox_messages DROP COLUMN fingerprint`},
			want: []string{"колонки fingerprint нет"},
		},
		{
			name: "у колонки чужой тип",
			ddl:  []string{`ALTER TABLE outbox_messages ALTER COLUMN dedup_key TYPE VARCHAR(64)`},
			want: []string{"колонка dedup_key: тип character varying, ожидается text"},
		},
		{
			name: "расхождений несколько — в одной ошибке",
			ddl: []string{
				`DROP INDEX ix_outbox_messages_failed`,
				`ALTER TABLE outbox_messages DROP CONSTRAINT outbox_messages_fail_chk`,
			},
			want: []string{"индекса ix_outbox_messages_failed нет", "ограничения outbox_messages_fail_chk нет"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			mustEnqueue(t, store, envelope())
			for _, ddl := range tt.ddl {
				_, err := pool.Exec(t.Context(), ddl)
				require.NoError(t, err)
			}

			err := store.CheckSchema(t.Context())

			require.Error(t, err)
			assert.Contains(t, err.Error(), "расходится с outboxpg/schema.sql")
			for _, want := range tt.want {
				assert.Contains(t, err.Error(), want)
			}
			assert.NotContains(t, err.Error(), secretPayload, "данных таблицы в ошибке нет")
		})
	}
}

// Своя колонка потребителя расхождением не считается: пакет проверяет то, чем
// пользуется, а не запрещает дополнять таблицу.
func TestCheckSchema_ExtraConsumerColumnIsFine(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	_, err := pool.Exec(t.Context(), `ALTER TABLE outbox_messages ADD COLUMN tenant_id UUID`)
	require.NoError(t, err)

	require.NoError(t, store.CheckSchema(t.Context()))
}
