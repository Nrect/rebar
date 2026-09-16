package entitlementpg_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// CheckSchema называет расхождение именем того, чего не хватает.
func TestCheckSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil string
		want  []string
	}{
		{
			name:  "снят потолок предмета",
			spoil: `ALTER TABLE entitlement_grants DROP CONSTRAINT ck_entitlement_grants_item_id`,
			want:  []string{"ограничения ck_entitlement_grants_item_id нет"},
		},
		{
			// ON CONFLICT встаёт на ключ по имени: под другим именем выдача сломается.
			name:  "первичный ключ под другим именем",
			spoil: `ALTER TABLE entitlement_grants RENAME CONSTRAINT ux_entitlement_grants_subject_item TO entitlement_grants_pkey`,
			want: []string{
				"ограничения ux_entitlement_grants_subject_item нет",
				"индекса ux_entitlement_grants_subject_item нет",
			},
		},
		{
			name:  "нет колонки момента выдачи",
			spoil: `ALTER TABLE entitlement_grants DROP COLUMN granted_at`,
			want:  []string{"колонки granted_at нет"},
		},
		{
			name:  "субъект строкой",
			spoil: `ALTER TABLE entitlement_grants ALTER COLUMN subject_id TYPE text USING subject_id::text`,
			want:  []string{"колонка subject_id: тип text, ожидается uuid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, pool := newStore(t)
			pgtest.Apply(t, pool, tt.spoil)

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "накатите entitlementpg.Migrations() раннером проекта",
				"первая строка — что делать")
			for _, want := range tt.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// Свои колонки потребитель добавлять вправе: это не расхождение, и выдача
// пишется дальше — адаптер перечисляет свои колонки явно.
func TestCheckSchema_IgnoresExtraColumn(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE entitlement_grants ADD COLUMN source text NOT NULL DEFAULT 'order'`)

	require.NoError(t, store.CheckSchema(t.Context()))
	require.NoError(t, store.Grant(t.Context(), uuid.New(), entitlement.Grant{ItemID: item}, moment()))
}

// Таблицы нет — первая строка ошибки говорит, что делать.
func TestCheckSchema_MissingTable(t *testing.T) {
	t.Parallel()

	err := entitlementpg.New(newSchemaPool(t)).CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "накатите entitlementpg.Migrations() раннером проекта")
}

// Все расхождения — в одной ошибке: чинить их по одному за прогон значило бы
// столько же миграций, сколько ошибок.
func TestCheckSchema_ReportsAllProblems(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE entitlement_grants DROP CONSTRAINT ck_entitlement_grants_item_id`)
	pgtest.Apply(t, pool, `ALTER TABLE entitlement_grants DROP COLUMN expires_at`)

	err := store.CheckSchema(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ограничения ck_entitlement_grants_item_id нет")
	assert.Contains(t, err.Error(), "колонки expires_at нет")
}

// Сбой запроса к каталогу — недоступность, а не «схема расходится».
func TestCheckSchema_FailureIsUnavailable(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, store.CheckSchema(ctx), entitlement.ErrUnavailable)
}
