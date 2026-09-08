package authzpg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Обе стороны миграции применяются на пустую базу: файл уезжает в миграции
// потребителя как есть.
func TestSchema_AppliesBothWays(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	require.NoError(t, authzpg.New(pool).CheckSchema(t.Context()))

	pgtest.Apply(t, pool, gooseDown(t))
	require.Error(t, authzpg.New(pool).CheckSchema(t.Context()), "после отката таблицы нет")
}

// Schema — тот же файл побайтно: потребитель, применяющий миграцию из кода,
// получает ровно то, что лежит в каталоге.
func TestSchema_EmbedMatchesFile(t *testing.T) {
	t.Parallel()

	assert.Equal(t, readSchema(t), authzpg.Schema)
}

// CheckSchema называет расхождения и не ругается на колонку потребителя.
func TestCheckSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil []string
		want  string
	}{
		{
			name:  "нет индекса уборки",
			spoil: []string{`DROP INDEX ix_authz_role_assignments_expires`},
			want:  "индекса ix_authz_role_assignments_expires нет",
		},
		{
			name:  "снято ограничение формы роли",
			spoil: []string{`ALTER TABLE authz_role_assignments DROP CONSTRAINT authz_role_assignments_role_chk`},
			want:  "ограничения authz_role_assignments_role_chk нет",
		},
		{
			name:  "нет колонки срока",
			spoil: []string{`ALTER TABLE authz_role_assignments DROP COLUMN expires_at`},
			want:  "колонки expires_at нет",
		},
		{
			name: "у выдавшего не тот тип",
			spoil: []string{
				`ALTER TABLE authz_role_assignments DROP CONSTRAINT authz_role_assignments_granted_by_chk`,
				`ALTER TABLE authz_role_assignments ALTER COLUMN granted_by DROP DEFAULT`,
				`ALTER TABLE authz_role_assignments ALTER COLUMN granted_by TYPE INT USING 0`,
			},
			want: "колонка granted_by: тип integer, ожидается text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			for _, sql := range tt.spoil {
				_, err := pool.Exec(t.Context(), sql)
				require.NoError(t, err)
			}

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "сверьте миграцию", "первая строка — что делать")
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// Свои колонки потребитель добавлять вправе, и расхождением это не считается.
func TestCheckSchema_IgnoresExtraColumn(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	_, err := pool.Exec(t.Context(), `ALTER TABLE authz_role_assignments ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
	require.NoError(t, err)

	assert.NoError(t, store.CheckSchema(t.Context()))
}

// Таблицы нет — первая строка ошибки говорит, что делать.
func TestCheckSchema_MissingTable(t *testing.T) {
	t.Parallel()
	pool := newSchemaPool(t)

	err := authzpg.New(pool).CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "скопируйте authzpg/schema.sql в миграции")
}

// Все расхождения — в одной ошибке: чинить их по одному прогону значило бы
// столько же миграций, сколько ошибок.
func TestCheckSchema_ReportsAllProblems(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)

	for _, sql := range []string{
		`DROP INDEX ix_authz_role_assignments_expires`,
		`ALTER TABLE authz_role_assignments DROP CONSTRAINT authz_role_assignments_role_chk`,
		`ALTER TABLE authz_role_assignments DROP COLUMN granted_by`,
	} {
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err)
	}

	err := store.CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "индекса ix_authz_role_assignments_expires нет")
	assert.Contains(t, err.Error(), "ограничения authz_role_assignments_role_chk нет")
	assert.Contains(t, err.Error(), "колонки granted_by нет")
}
