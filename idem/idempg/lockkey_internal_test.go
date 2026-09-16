package idempg

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Эталоны посчитаны вне Go (Python: hashlib.sha256 и struct.pack(">q", len)
// у каждого поля, первые восемь байт — ">q"). Формула — контракт между версиями
// на выкате: разошедшийся ключ исполнил бы параллельный повтор дважды.
func TestLockKey_GoldenVector(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		realm, subject, key string
		want                int64
	}{
		{"customers", "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f", "k-1", -803734920466142981},
		{"staff", "Иван Петров", "order;2026-09-16,T12:00~!", -395103905478083517},
		{"", "", "", 4334871117923156580},
		{"z", "s", strings.Repeat("~", 255), -14066496589890466},
	} {
		assert.Equal(t, tc.want, lockKey(tc.realm, tc.subject, tc.key), "%q %q %q", tc.realm, tc.subject, tc.key)
	}
}

// Без префикса длины соседние поля склеивались бы, и две области с одним
// ключом держали бы одну блокировку. Пары с нулевыми байтами внутри поля ловят
// и постоянный разделитель вместо длины.
func TestLockKey_FieldBoundariesDoNotCollide(t *testing.T) {
	t.Parallel()

	nul := strings.Repeat("\x00", 8)
	for _, pair := range [][2][3]string{
		{{"ab", "c", "k"}, {"a", "bc", "k"}},
		{{"r", "ab", "c"}, {"r", "a", "bc"}},
		{{"a" + nul + "b", "c", "k"}, {"a", "b" + nul + "c", "k"}},
		{{"r", "s" + nul, "k"}, {"r", "s", nul + "k"}},
	} {
		first, second := pair[0], pair[1]
		assert.NotEqual(t, lockKey(first[0], first[1], first[2]), lockKey(second[0], second[1], second[2]),
			"%q и %q", first, second)
	}
}
