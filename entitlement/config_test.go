package entitlement_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementtest"
)

// Каждое обязательное поле — своя строка: ошибка проводки обязана падать на
// старте процесса и НАЗЫВАТЬ ПОЛЕ, а не «invalid config».
func TestConfig_RejectsEveryHole(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want string
		tune func(*entitlement.Config)
	}{
		{"нулевое значение целиком", "Config.TTL must be positive", func(c *entitlement.Config) { *c = entitlement.Config{} }},
		{"нулевой TTL", "Config.TTL must be positive", func(c *entitlement.Config) { c.TTL = 0 }},
		{"отрицательный TTL", "Config.TTL must be positive", func(c *entitlement.Config) { c.TTL = -time.Second }},
		{"нулевой MaxSubjects", "Config.MaxSubjects must be positive", func(c *entitlement.Config) { c.MaxSubjects = 0 }},
		{"отрицательный MaxSubjects", "Config.MaxSubjects must be positive", func(c *entitlement.Config) { c.MaxSubjects = -1 }},
		{"нулевой LoadTimeout", "Config.LoadTimeout must be positive", func(c *entitlement.Config) { c.LoadTimeout = 0 }},
		{"отрицательный LoadTimeout", "Config.LoadTimeout must be positive", func(c *entitlement.Config) { c.LoadTimeout = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tc.tune(&cfg)
			requirePanics(t, tc.want, func() { entitlement.New(entitlementtest.NewMemStore(), cfg) })
		})
	}

	assert.NotPanics(t, func() { entitlement.New(entitlementtest.NewMemStore(), validConfig()) })
}

// Единица в каждом поле законна: валидатор проверяет знак, а не «выглядит
// разумно». Разумность — дело потребителя, и TTL в наносекунду это законный
// режим «всегда в базу», ради которого не нужен флаг.
func TestConfig_MinimalValuesAreLegal(t *testing.T) {
	t.Parallel()

	cfg := entitlement.Config{TTL: 1, MaxSubjects: 1, LoadTimeout: 1}
	assert.NotPanics(t, func() { entitlement.New(entitlementtest.NewMemStore(), cfg) })
}

// ФЛАГОВ-ВЫКЛЮЧАТЕЛЕЙ ИНВАРИАНТОВ НЕТ. Поле вида SkipCache или TrustStore
// рано или поздно окажется включённым в проде, и это будет не баг, а
// разрешённая конфигурация (CONVENTIONS §2). Проверяется по составу типа,
// чтобы правило пережило любую будущую правку Config.
func TestConfig_HasNoInvariantSwitches(t *testing.T) {
	t.Parallel()

	forbidden := []string{"skip", "trust", "allow", "disable", "insecure", "unsafe"}
	cfg := reflect.TypeOf(entitlement.Config{})
	for i := range cfg.NumField() {
		name := strings.ToLower(cfg.Field(i).Name)
		for _, bad := range forbidden {
			assert.NotContains(t, name, bad, "поле %s похоже на выключатель инварианта", cfg.Field(i).Name)
		}
		assert.NotEqual(t, reflect.Bool, cfg.Field(i).Type.Kind(), "булево поле в Config — это флаг режима")
	}
}

// requirePanics — конструктор обязан отвергнуть негодное на старте, и текст
// паники обязан называть поле.
func requirePanics(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "негодная конфигурация обязана быть отвергнута")
		text, ok := r.(string)
		require.True(t, ok, "паника обязана быть строкой")
		assert.Contains(t, text, want)
	}()
	fn()
}
