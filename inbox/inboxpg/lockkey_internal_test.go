package inboxpg

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/inbox"
)

// Золотые значения посчитаны вне Go — формула ключа не выводится из самого кода:
//
//	python3 -c 'import hashlib,struct;h=hashlib.sha256();[h.update(struct.pack(">q",len(f))+f) for f in (b"rebar/inbox/lock/v1",b"billing",b"evt_1")];print(struct.unpack(">q",h.digest()[:8])[0])'
//
// Реплики разных версий на выкате обязаны посчитать один ключ: разойдись формула —
// параллельный дубль ждёт на индексе вместо in_flight.
func TestLockKey_Golden(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		source inbox.SourceName
		id     string
		want   int64
	}{
		{"billing", "evt_1", 7315998742185353857},
		{"billing", "evt_2", -390798276003009787}, // старший бит взведён: перенос в знак
		{"suite_main", "order.paid:ord_42:paid", 5640211344315314470},
	} {
		assert.Equal(t, tc.want, lockKey(tc.source, tc.id), "ключ (%s, %s)", tc.source, tc.id)
	}
}

// Префикс длины у каждого поля. Первая пара ловит ключ без префикса, вторая —
// постоянный префикс: восемь нулевых байт — тот же разделитель, и поле с ними
// внутри склеивается с соседним.
func TestLockKey_FieldBoundaries(t *testing.T) {
	t.Parallel()

	zeros := strings.Repeat("\x00", 8)
	type field struct {
		source inbox.SourceName
		id     string
		want   int64
	}
	for _, pair := range [][2]field{
		{{"ab", "c", 8253197976405170468}, {"a", "bc", 3572648023934752508}},
		{{inbox.SourceName("ab" + zeros + "c"), "d", 6695392211046687248}, {"ab", "c" + zeros + "d", 2469850344083051149}},
	} {
		first, second := lockKey(pair[0].source, pair[0].id), lockKey(pair[1].source, pair[1].id)
		assert.NotEqual(t, first, second, "поля %q и %q склеились", pair[0].source, pair[1].source)
		assert.Equal(t, pair[0].want, first, "ключ (%q, %q)", pair[0].source, pair[0].id)
		assert.Equal(t, pair[1].want, second, "ключ (%q, %q)", pair[1].source, pair[1].id)
	}
}
