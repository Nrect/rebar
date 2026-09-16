package idem_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

func TestParseKey_Accepts(t *testing.T) {
	t.Parallel()

	longest := strings.Repeat("k", idem.MaxKeyLen)
	for _, tc := range []struct {
		line, want string
	}{
		{"7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f", "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"},
		{`"7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"`, "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"},
		{"AbC", "AbC"},
		{"a", "a"},
		{longest, longest},
		{`"` + longest + `"`, longest},
		{"!#$%&'()*+-./:<=>?@[]^_`{|}~", "!#$%&'()*+-./:<=>?@[]^_`{|}~"},
		{`"a;b,c"`, "a;b,c"},
		{"?1", "?1"},
		{"123", "123"},
	} {
		key, err := idem.ParseKey(tc.line)
		require.NoError(t, err, "%q", tc.line)
		assert.Equal(t, tc.want, key.String(), "%q", tc.line)
		assert.False(t, key.IsZero(), "%q", tc.line)
	}
}

// Строка Structured Fields и голое значение — один и тот же ключ.
func TestParseKey_QuotedAndBareAreOneKey(t *testing.T) {
	t.Parallel()

	bare, err := idem.ParseKey("order-17")
	require.NoError(t, err)
	quoted, err := idem.ParseKey(`"order-17"`)
	require.NoError(t, err)
	assert.Equal(t, bare, quoted)

	other, err := idem.ParseKey("Order-17")
	require.NoError(t, err)
	assert.NotEqual(t, bare, other, "регистр не меняется: ключ непрозрачен")
}

func TestParseKey_Missing(t *testing.T) {
	t.Parallel()

	for _, lines := range [][]string{nil, {}} {
		key, err := idem.ParseKey(lines...)
		require.ErrorIs(t, err, idem.ErrKeyMissing)
		require.NotErrorIs(t, err, idem.ErrKeyInvalid)
		assert.True(t, key.IsZero())
	}
}

func TestParseKey_Rejects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		what  string
		lines []string
	}{
		{"пустая строка поля", []string{""}},
		{"пустая строка SF", []string{`""`}},
		{"одна кавычка", []string{`"`}},
		{"строка не закрыта", []string{`"abc`}},
		{"кавычка в конце голого", []string{`abc"`}},
		{"длиннее потолка", []string{strings.Repeat("k", idem.MaxKeyLen+1)}},
		{"длиннее потолка в кавычках", []string{`"` + strings.Repeat("k", idem.MaxKeyLen+1) + `"`}},
		{"пробел внутри", []string{"a b"}},
		{"пробел внутри кавычек", []string{`"a b"`}},
		{"пробел в начале", []string{" abc"}},
		{"пробел в конце", []string{"abc "}},
		{"табуляция", []string{"a\tb"}},
		{"перевод строки", []string{"a\nb"}},
		{"NUL", []string{"a\x00b"}},
		{"DEL", []string{"a\x7fb"}},
		{"не ASCII", []string{"ключ"}},
		{"обратная косая черта", []string{`a\b`}},
		{"экранирование в кавычках", []string{`"a\"b"`}},
		{"кавычка внутри кавычек", []string{`"a"b"`}},
		{"параметр у голого", []string{"abc;x=y"}},
		{"параметр у строки", []string{`"abc";x=y`}},
		{"флаг-параметр", []string{"abc;x"}},
		{"список голых", []string{"a,b"}},
		{"список строк", []string{`"a", "b"`}},
		{"две строки поля", []string{"a", "b"}},
		{"две одинаковые строки поля", []string{"a", "a"}},
		{"строка поля и пустая строка", []string{"a", ""}},
		{"строка SF с пробелом после", []string{`"abc" `}},
	} {
		key, err := idem.ParseKey(tc.lines...)
		assert.True(t, key.IsZero(), tc.what)
		assert.ErrorIs(t, err, idem.ErrKeyInvalid, tc.what)
	}
}

// Свойства разбора на произвольной строке поля: разбор тотален, форма ключа —
// контракт со схемой, строка SF и голое значение дают один ключ. Дым —
// -fuzztime=30s; длинный прогон — на мощном ПК.
func FuzzParseKey(f *testing.F) {
	for _, seed := range []string{
		"", "a", `"a"`, `""`, `"`, "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f", "a b", "a;b", `"a;b"`, "a,b",
		`"a\"b"`, `a\b`, "ключ", strings.Repeat("k", idem.MaxKeyLen), strings.Repeat("k", idem.MaxKeyLen+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		key, err := idem.ParseKey(line)
		if err != nil {
			require.ErrorIs(t, err, idem.ErrKeyInvalid, "одна строка поля — только ErrKeyInvalid")
			require.True(t, key.IsZero())
			return
		}
		value := key.String()
		require.NotEmpty(t, value)
		require.LessOrEqual(t, len(value), idem.MaxKeyLen)
		for i := range len(value) {
			c := value[i]
			require.True(t, c >= '!' && c <= '~' && c != '"' && c != '\\', "байт %d ключа %q вне формы схемы", i, value)
		}

		again, err := idem.ParseKey(`"` + value + `"`)
		require.NoError(t, err, "ключ %q в кавычках не разобрался", value)
		require.Equal(t, key, again, "строка SF дала другой ключ")
		if !strings.ContainsAny(value, ";,") {
			bare, bareErr := idem.ParseKey(value)
			require.NoError(t, bareErr, "голый ключ %q не разобрался", value)
			require.Equal(t, key, bare, "голое значение дало другой ключ")
		}
		_, err = idem.ParseKey(line, line)
		require.ErrorIs(t, err, idem.ErrKeyInvalid, "две строки поля")
	})
}
