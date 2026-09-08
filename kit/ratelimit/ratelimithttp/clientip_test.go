package ratelimithttp_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/ratelimit/ratelimithttp"
)

// trusted — прокси собственной инфраструктуры: только их X-Forwarded-For
// имеет вес.
var trusted = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fe80::/10"),
}

func request(remote string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.RemoteAddr = remote
	for _, value := range forwarded {
		r.Header.Add("X-Forwarded-For", value)
	}
	return r
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		remote    string
		forwarded []string
		want      string
	}{
		{name: "без заголовка — сосед по соединению", remote: "203.0.113.7:33333", want: "203.0.113.7"},
		{
			name:      "подделка от недоверенного клиента игнорируется",
			remote:    "203.0.113.7:33333",
			forwarded: []string{"1.1.1.1"},
			want:      "203.0.113.7",
		},
		{
			name:      "заголовок доверенного прокси",
			remote:    "10.0.0.1:443",
			forwarded: []string{"203.0.113.7"},
			want:      "203.0.113.7",
		},
		{
			name:      "цепочка читается справа налево",
			remote:    "10.0.0.1:443",
			forwarded: []string{"203.0.113.7, 10.0.0.5"},
			want:      "203.0.113.7",
		},
		{
			name:      "подделка слева от настоящего адреса не считается",
			remote:    "10.0.0.1:443",
			forwarded: []string{"1.1.1.1, 198.51.100.9, 10.0.0.5"},
			want:      "198.51.100.9",
		},
		{
			name:      "вся цепочка доверенная — крайний левый",
			remote:    "10.0.0.1:443",
			forwarded: []string{"10.1.1.1, 10.2.2.2"},
			want:      "10.1.1.1",
		},
		{
			name:      "мусор обрывает разбор",
			remote:    "10.0.0.1:443",
			forwarded: []string{"1.1.1.1, мусор, 10.0.0.5"},
			want:      "10.0.0.5",
		},
		{
			name:      "запись с портом — тоже мусор",
			remote:    "10.0.0.1:443",
			forwarded: []string{"203.0.113.7:9999"},
			want:      "10.0.0.1",
		},
		{
			name:      "пустая запись обрывает разбор",
			remote:    "10.0.0.1:443",
			forwarded: []string{""},
			want:      "10.0.0.1",
		},
		{
			name:      "заголовок пришёл двумя строками",
			remote:    "10.0.0.1:443",
			forwarded: []string{"203.0.113.7", "10.0.0.5"},
			want:      "203.0.113.7",
		},
		{
			name:      "RemoteAddr без порта",
			remote:    "10.0.0.1",
			forwarded: []string{"203.0.113.7"},
			want:      "203.0.113.7",
		},
		{
			name:      "IPv4 в форме ::ffff: считается тем же адресом",
			remote:    "[::ffff:10.0.0.1]:443",
			forwarded: []string{"::ffff:203.0.113.7"},
			want:      "203.0.113.7",
		},
		{
			name:      "IPv6-клиент за IPv6-прокси",
			remote:    "[2001:db8::1]:443",
			forwarded: []string{"2606:4700::1111, 2001:db8::2"},
			want:      "2606:4700::1111",
		},
		{name: "зона IPv6 отбрасывается", remote: "[fe80::1%eth0]:443", want: "fe80::1"},
		{name: "негодный RemoteAddr", remote: "не адрес", want: ""},
		{name: "пустой RemoteAddr", remote: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ratelimithttp.ClientIP(request(tc.remote, tc.forwarded...), trusted))
		})
	}
}

// Без доверенных прокси заголовок не читается никогда: конфигурация по
// умолчанию (пустой список) обязана быть безопасной.
func TestClientIP_NoTrustedProxiesIgnoresHeader(t *testing.T) {
	t.Parallel()

	r := request("10.0.0.1:443", "203.0.113.7")
	assert.Equal(t, "10.0.0.1", ratelimithttp.ClientIP(r, nil))
	assert.Equal(t, "10.0.0.1", ratelimithttp.ClientIP(r, []netip.Prefix{}))
}

func TestByIP(t *testing.T) {
	t.Parallel()

	key := ratelimithttp.ByIP(trusted, 64)

	assert.Equal(t, "203.0.113.7", key(request("203.0.113.7:33333")), "IPv4 — адрес целиком")
	assert.Equal(t, "2606:4700:0:1::/64", key(request("[2606:4700:0:1::5]:443")))
	assert.Empty(t, key(request("не адрес")), "нет адреса — нет ключа: лимитер откажет")
}

// Соседние адреса одной подсети /64 обязаны делить лимит: их у клиента 2^64,
// и полный адрес дал бы burst каждому.
func TestByIP_AggregatesIPv6BySubnet(t *testing.T) {
	t.Parallel()

	key := ratelimithttp.ByIP(trusted, 64)

	first := key(request("[2606:4700:0:1::5]:443"))
	second := key(request("[2606:4700:0:1:dead:beef:1:2]:443"))
	other := key(request("[2606:4700:0:2::5]:443"))

	require.NotEmpty(t, first)
	assert.Equal(t, first, second, "один /64 — один ключ")
	assert.NotEqual(t, first, other, "разные /64 — разные ключи")
}

func TestByIP_PanicsOnImpossiblePrefix(t *testing.T) {
	t.Parallel()

	for _, bits := range []int{0, -1, 129} {
		assert.PanicsWithValuef(t, "ratelimithttp.ByIP: v6PrefixBits must be in [1, 128]",
			func() { ratelimithttp.ByIP(trusted, bits) }, "%d бит", bits)
	}
	// Края допустимого диапазона законны: /1 — «весь интернет одним ключом»,
	// /128 — адрес целиком.
	for _, bits := range []int{1, 128} {
		assert.NotPanicsf(t, func() { ratelimithttp.ByIP(trusted, bits) }, "%d бит", bits)
	}
}
