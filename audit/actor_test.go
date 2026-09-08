package audit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
)

// Пустой контекст актора не даёт: «не положили» отличимо от «аноним».
func TestActorFrom_EmptyContext(t *testing.T) {
	t.Parallel()

	_, ok := audit.ActorFrom(t.Context())
	assert.False(t, ok)
}

func TestNewContext_RoundTrip(t *testing.T) {
	t.Parallel()

	want := audit.Actor{Kind: audit.ActorSystem, ID: "cron", Name: "purge"}
	got, ok := audit.ActorFrom(audit.NewContext(t.Context(), want))
	require.True(t, ok)
	assert.Equal(t, want, got)
}

// Ключ контекста свой и неэкспортируемый: чужое значение по строковому ключу
// актора не подменяет.
func TestActorFrom_IgnoresForeignKey(t *testing.T) {
	t.Parallel()

	// Подделка ключом чужого типа — предмет проверки, поэтому оба стража тут молчат.
	//nolint:revive,staticcheck // строковый ключ здесь намеренный
	ctx := context.WithValue(t.Context(), "actor", audit.Actor{Kind: audit.ActorUser, ID: "подделка"})
	_, ok := audit.ActorFrom(ctx)
	assert.False(t, ok, "актор берётся только из NewContext")
}

// Вложенный NewContext перекрывает прежнего актора: обвязка входа ставит
// субъекта после аутентификации, поверх анонима.
func TestNewContext_InnerActorWins(t *testing.T) {
	t.Parallel()

	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorAnonymous})
	ctx = audit.NewContext(ctx, audit.Actor{Kind: audit.ActorUser, ID: "u-1"})

	got, ok := audit.ActorFrom(ctx)
	require.True(t, ok)
	assert.Equal(t, audit.ActorUser, got.Kind)
}
