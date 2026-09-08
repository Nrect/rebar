package audit_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/audittest"
)

// Негодный Config — паника на старте, а не отказ на первом событии
// (CORRECTNESS, закон 9). По полю на строку: пропущенная проверка иначе
// всплывает у потребителя.
func TestNewRecorder_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mod  func(*audit.Config)
		want string
	}{
		{name: "нет действий", mod: func(c *audit.Config) { c.Actions = nil }, want: "Config.Actions must not be empty"},
		{
			name: "действие не по алфавиту",
			mod:  func(c *audit.Config) { c.Actions = []audit.Action{"User.Login"} },
			want: `Config.Actions: action "User.Login" must match`,
		},
		{
			name: "пустое действие",
			mod:  func(c *audit.Config) { c.Actions = []audit.Action{""} },
			want: `Config.Actions: action "" must match`,
		},
		{
			name: "действие длиннее потолка",
			mod: func(c *audit.Config) {
				c.Actions = []audit.Action{audit.Action(strings.Repeat("a", audit.MaxActionLen+1))}
			},
			want: "must match",
		},
		{
			name: "действие дважды",
			mod:  func(c *audit.Config) { c.Actions = []audit.Action{actionLogin, actionLogin} },
			want: "is listed twice",
		},
		{name: "нулевой MaxDetails", mod: func(c *audit.Config) { c.MaxDetails = 0 }, want: "Config.MaxDetails must be positive"},
		{
			name: "отрицательный MaxDetails",
			mod:  func(c *audit.Config) { c.MaxDetails = -1 },
			want: "Config.MaxDetails must be positive",
		},
		{
			name: "нулевой MaxDetailLen",
			mod:  func(c *audit.Config) { c.MaxDetailLen = 0 },
			want: "Config.MaxDetailLen must be positive",
		},
		{
			name: "отрицательный MaxDetailLen",
			mod:  func(c *audit.Config) { c.MaxDetailLen = -1 },
			want: "Config.MaxDetailLen must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tt.mod(&cfg)
			got := recoverPanic(t, func() { audit.NewRecorder(audittest.NewSink(), cfg) })
			assert.Contains(t, got, "audit.NewRecorder: ")
			assert.Contains(t, got, tt.want)
		})
	}
}

// Действие ровно в потолок длины годится: граница включающая.
func TestNewRecorder_AcceptsActionAtLengthLimit(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Actions = []audit.Action{audit.Action(strings.Repeat("a", audit.MaxActionLen))}
	assert.NotPanics(t, func() { audit.NewRecorder(audittest.NewSink(), cfg) })
}

// Nil-приёмник — паника: собранный с ним Recorder молча съедал бы журнал.
func TestNewRecorder_PanicsOnNilSink(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "audit.NewRecorder: sink must not be nil", func() {
		audit.NewRecorder(nil, testConfig())
	})
}

// Флага, выключающего инвариант, в Config нет: режим разработки — подмена
// приёмника, а не Skip*-поле (CORRECTNESS, закон 9).
func TestConfig_HasNoInvariantSwitches(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(audit.Config{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		for _, bad := range []string{"Skip", "Trust", "Allow", "Disable", "Unsafe"} {
			assert.NotContains(t, name, bad, "поле %s выключает инвариант", name)
		}
	}
}

// recoverPanic — текст паники fn; тест падает, если паники не было.
func recoverPanic(t *testing.T, fn func()) string {
	t.Helper()
	var got string
	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r, "ожидалась паника")
			text, ok := r.(string)
			require.True(t, ok, "паника обязана быть строкой")
			got = text
		}()
		fn()
	}()
	return got
}
