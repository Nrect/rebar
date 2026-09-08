package authz_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
)

// Негодное правило роняет сборку на старте: реестр, который можно прочитать
// двумя способами, рано или поздно прочитают вторым.
func TestRegistry_PanicsOnInvalidRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   authz.Operation
		rule authz.Rule
		want string
	}{
		{
			name: "публичная без объяснения",
			op:   opPublic,
			rule: authz.Rule{Public: true},
			want: "must explain itself in Rule.Why",
		},
		{
			name: "объяснение из пробелов",
			op:   opPublic,
			rule: authz.Rule{Public: true, Why: "   \t\n"},
			want: "must explain itself in Rule.Why",
		},
		{
			name: "публичная и с разрешением сразу",
			op:   opPublic,
			rule: authz.Rule{Public: true, Why: "проба живости", Permission: permRead},
			want: "must not also require permission",
		},
		{
			name: "непубличная без разрешения",
			op:   opRead,
			rule: authz.Rule{},
			want: "must name a Permission or be Public with Why",
		},
		{
			name: "разрешение не из Config",
			op:   opRead,
			rule: authz.Rule{Permission: "order.delete"},
			want: "requires permission \"order.delete\" that is not in Config.Permissions",
		},
		{
			name: "пустое имя операции",
			op:   "",
			rule: authz.Rule{Permission: permRead},
			want: "must be non-empty",
		},
		{
			name: "управляющий символ в имени операции",
			op:   "GET /orders\n",
			rule: authz.Rule{Permission: permRead},
			want: "free of control characters",
		},
		{
			name: "имя операции длиннее потолка",
			op:   authz.Operation(strings.Repeat("x", authz.MaxOperationLen+1)),
			rule: authz.Rule{Permission: permRead},
			want: "at most 128 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requirePanics(t, tt.want, func() {
				authz.NewRegistry(validConfig(), map[authz.Operation]authz.Rule{tt.op: tt.rule})
			})
		})
	}
}

// Реестр строится на проверенном Config: негодная модель прав роняет и его.
func TestRegistry_PanicsOnInvalidConfig(t *testing.T) {
	t.Parallel()

	requirePanics(t, "Config.Permissions must not be empty", func() {
		authz.NewRegistry(authz.Config{}, nil)
	})
}

// Пустой реестр законен и означает «операций нет, запрещено всё».
func TestRegistry_EmptyDeniesEverything(t *testing.T) {
	t.Parallel()

	reg := authz.NewRegistry(validConfig(), nil)
	assert.Empty(t, reg.Operations())

	_, ok := reg.Rule(opRead)
	assert.False(t, ok)
}

// Карта правил копируется: правка карты вызывающего после NewRegistry не
// должна открывать операцию в работающем процессе.
func TestRegistry_CopiesRules(t *testing.T) {
	t.Parallel()

	rules := validRules()
	reg := authz.NewRegistry(validConfig(), rules)
	rules[opUnknown] = authz.Rule{Public: true, Why: "дописано после сборки"}

	_, ok := reg.Rule(opUnknown)
	assert.False(t, ok, "правка карты после NewRegistry открыла операцию")
	assert.Equal(t, []authz.Operation{opPublic, opRead}, reg.Operations())
}

// Правило возвращается как есть: по нему потребитель пишет аудит и ревью.
func TestRegistry_Rule(t *testing.T) {
	t.Parallel()

	reg := authz.NewRegistry(validConfig(), validRules())

	rule, ok := reg.Rule(opPublic)
	require.True(t, ok)
	assert.True(t, rule.Public)
	assert.NotEmpty(t, rule.Why)
	assert.Empty(t, rule.Permission)

	rule, ok = reg.Rule(opRead)
	require.True(t, ok)
	assert.False(t, rule.Public)
	assert.Equal(t, permRead, rule.Permission)
}

// Объяснение у непубличной операции не запрещено: оно полезно на ревью.
func TestRegistry_WhyIsAllowedOnPrivateRule(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		authz.NewRegistry(validConfig(), map[authz.Operation]authz.Rule{
			opRead: {Permission: permRead, Why: "витрина заказов сотрудника"},
		})
	})
}
