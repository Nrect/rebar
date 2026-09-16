package inbox_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/kit/errs"
)

// Допуск — в обе стороны и включительно; подпись из будущего дальше допуска —
// тот же отказ, что и старая: заранее подписанный запрос не ждёт своего часа.
func TestCheckTimestamp(t *testing.T) {
	t.Parallel()

	const tolerance = 5 * time.Minute
	for _, tc := range []struct {
		shift time.Duration
		ok    bool
	}{
		{0, true},
		{-tolerance, true},
		{tolerance, true},
		{-tolerance - time.Nanosecond, false},
		{tolerance + time.Nanosecond, false},
		{-100 * 365 * 24 * time.Hour, false},
	} {
		err := inbox.CheckTimestamp(start.Add(tc.shift), start, tolerance)
		if tc.ok {
			require.NoError(t, err, "сдвиг %s", tc.shift)
			continue
		}
		require.ErrorIs(t, err, inbox.ErrNotAuthentic, "сдвиг %s", tc.shift)
		assert.Equal(t, errs.KindIncorrectInput, errs.KindOf(err))
	}
	require.ErrorIs(t, inbox.CheckTimestamp(time.Time{}, start, tolerance), inbox.ErrNotAuthentic, "нулевой момент")
}

// Нулевой допуск у Stripe выключает проверку свежести целиком: у нас это паника.
func TestCheckTimestamp_ZeroTolerancePanics(t *testing.T) {
	t.Parallel()

	for _, tolerance := range []time.Duration{0, -time.Second} {
		assert.PanicsWithValue(t, "inbox.CheckTimestamp: tolerance must be positive: zero would disable the freshness check",
			func() { _ = inbox.CheckTimestamp(start, start, tolerance) })
	}
}

func TestAddrIn(t *testing.T) {
	t.Parallel()

	nets := []netip.Prefix{netip.MustParsePrefix("185.71.76.0/27"), netip.MustParsePrefix("2a02:5180::/32")}
	for _, tc := range []struct {
		remote string
		in     bool
	}{
		{"185.71.76.1", true},
		{"185.71.76.31", true},
		{"185.71.76.32", false},
		{"::ffff:185.71.76.1", true},
		{"2a02:5180::1", true},
		{"2a02:5180::1%eth0", true},
		{"2a02:5181::1", false},
		{"", false},
		{"garbage", false},
		{"185.71.76.1:443", false},
		{"[2a02:5180::1]:443", false},
		{" 185.71.76.1", false},
	} {
		assert.Equal(t, tc.in, inbox.AddrIn(tc.remote, nets), "адрес %q", tc.remote)
	}
	assert.False(t, inbox.AddrIn("185.71.76.1", nil), "без сетей проверять нечем — отказ")
	assert.False(t, inbox.AddrIn("185.71.76.1", []netip.Prefix{{}}), "негодная сеть не содержит ничего")
}

// FuzzAddrIn — AddrIn против независимого расчёта по байтам: адрес в сети,
// если совпали первые bits бит у приведённых 16-байтовых форм одного семейства.
func FuzzAddrIn(f *testing.F) {
	for _, seed := range []struct {
		remote string
		prefix string
	}{
		{"185.71.76.1", "185.71.76.0/27"},
		{"::ffff:185.71.76.1", "185.71.76.0/27"},
		{"2a02:5180::1%eth0", "2a02:5180::/32"},
		{"", "0.0.0.0/0"},
		{"::ffff:0.0.0.0", "::/0"},
		{"1.2.3.4:80", "1.2.3.0/24"},
	} {
		f.Add(seed.remote, seed.prefix)
	}

	f.Fuzz(func(t *testing.T, remote, prefix string) {
		network, err := netip.ParsePrefix(prefix)
		if err != nil {
			network = netip.Prefix{}
		}
		got := inbox.AddrIn(remote, []netip.Prefix{network})
		assert.Equal(t, referenceIn(remote, network), got, "адрес %q, сеть %q", remote, prefix)
		assert.False(t, inbox.AddrIn(remote, nil), "без сетей — всегда отказ")

		addr, parseErr := netip.ParseAddr(remote)
		if parseErr != nil {
			assert.False(t, got, "негодный адрес %q принят", remote)
			return
		}
		if addr.Is4() {
			mapped := netip.AddrFrom16(addr.As16()).String()
			assert.Equal(t, got, inbox.AddrIn(mapped, []netip.Prefix{network}), "IPv4 %q и его форма %q", remote, mapped)
		}
	})
}

// referenceIn — «в сети» без netip.Prefix.Contains: семейство адреса после
// снятия IPv4-в-IPv6 совпадает с семейством сети, и первые bits бит равны.
func referenceIn(remote string, network netip.Prefix) bool {
	addr, err := netip.ParseAddr(remote)
	if err != nil || !network.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.Is4() != network.Addr().Is4() || network.Addr().Zone() != "" {
		return false
	}
	a, n := addr.AsSlice(), network.Addr().AsSlice()
	bits := network.Bits()
	for i := range bits {
		mask := byte(0x80) >> (i % 8)
		if a[i/8]&mask != n[i/8]&mask {
			return false
		}
	}
	return true
}
