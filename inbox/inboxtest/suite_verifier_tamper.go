package inboxtest

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/nrect/rebar/inbox"
)

// maxFlips — потолок изменённых позиций на одно значение: длинное тело
// проверяется равномерной выборкой с первым и последним байтом.
const maxFlips = 512

func verifyBodyTamper(t reporter, c verifierCase) {
	t.Helper()
	if c.fx.BodyUnsigned {
		return
	}
	req := c.fx.Sign(1, suiteNow)
	want := c.mustVerify(t, req, "доставка до правки тела")
	for _, pos := range flipPositions(len(req.Raw)) {
		tampered := cloneRequest(req)
		tampered.Raw[pos] ^= 0x01
		c.refusedOrSame(t, want, tampered, fmt.Sprintf("байт тела %d из %d изменён", pos, len(req.Raw)))
	}
}

func verifyBodyGarbage(t reporter, c verifierCase) {
	t.Helper()
	if c.fx.BodyUnsigned {
		return
	}
	req := c.fx.Sign(1, suiteNow)
	want := c.mustVerify(t, req, "доставка до замены тела")
	for _, junk := range [][]byte{nil, {}, []byte("{}"), []byte("null"), []byte("[]"), {0x00, 0xff}} {
		tampered := cloneRequest(req)
		tampered.Raw = junk
		c.refusedOrSame(t, want, tampered, fmt.Sprintf("тело заменено на %q", junk))
	}
}

func verifyHeaderGarbage(t reporter, c verifierCase) {
	t.Helper()
	req := c.fx.Sign(1, suiteNow)
	want := c.mustVerify(t, req, "доставка до правки заголовков")
	bare := cloneRequest(req)
	bare.Headers = nil
	c.refusedOrSame(t, want, bare, "без заголовков")
	for _, name := range slices.Sorted(maps.Keys(req.Headers)) {
		c.refusedOrSame(t, want, withHeader(req, name, nil), "без заголовка "+name)
		c.refusedOrSame(t, want, withHeader(req, name, append(slices.Clone(req.Headers[name]), req.Headers[name]...)),
			"заголовок "+name+" дважды")
		for _, junk := range []string{"", "garbage", "\x00\xff", "t=,v1=", "v1,", strings.Repeat("A", 4096)} {
			c.refusedOrSame(t, want, withHeader(req, name, []string{junk}), fmt.Sprintf("заголовок %s = %q", name, clip(junk)))
		}
		for i, value := range req.Headers[name] {
			for _, pos := range flipPositions(len(value)) {
				values := slices.Clone(req.Headers[name])
				flipped := []byte(value)
				flipped[pos] ^= 0x01
				values[i] = string(flipped)
				c.refusedOrSame(t, want, withHeader(req, name, values), fmt.Sprintf("байт %d заголовка %s изменён", pos, name))
			}
		}
	}
}

// verifyAddress — только у схемы, которая кладёт адрес: пустой, негодный, с
// портом и чужой — отказ; IPv4 внутри IPv6 и адрес с зоной — то же событие.
func verifyAddress(t reporter, c verifierCase) {
	t.Helper()
	req := c.fx.Sign(1, suiteNow)
	if req.RemoteIP == "" {
		return
	}
	want := c.mustVerify(t, req, "доставка с адреса отправителя")
	addr, err := netip.ParseAddr(req.RemoteIP)
	if err != nil {
		t.Fatalf("Sign кладёт негодный адрес %q", clip(req.RemoteIP))
	}
	for _, remote := range []string{"", "garbage", netip.AddrPortFrom(addr, 443).String(), foreignAddr(addr).String()} {
		c.mustRefuse(t, withRemote(req, remote), fmt.Sprintf("адрес %q", remote))
	}
	normalized := []string{addr.String() + "%eth0"}
	if addr.Is4() {
		normalized = []string{netip.AddrFrom16(addr.As16()).String()}
	}
	for _, remote := range normalized {
		got := c.mustVerify(t, withRemote(req, remote), fmt.Sprintf("адрес отправителя в форме %q", remote))
		isTrue(t, sameEvent(got, want), fmt.Sprintf("адрес %q дал другое событие", remote))
	}
}

func verifyParallel(t reporter, c verifierCase) {
	t.Helper()
	const workers, rounds = 8, 20
	reqs := make([]inbox.Request, workers)
	wants := make([]inbox.Event, workers)
	for i := range workers {
		reqs[i] = c.fx.Sign(i+1, suiteNow)
		wants[i] = c.mustVerify(t, cloneRequest(reqs[i]), fmt.Sprintf("доставка %d", i+1))
	}
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			for range rounds {
				r := c.verify(t, cloneRequest(reqs[i]), fmt.Sprintf("параллельная доставка %d", i+1))
				if !r.panicked && (r.err != nil || !sameEvent(r.ev, wants[i])) {
					t.Errorf("параллельная доставка %d: другой исход при параллельных вызовах: %v", i+1, r.err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// flipPositions — позиции для правки значения длины n.
func flipPositions(n int) []int {
	if n <= maxFlips {
		positions := make([]int, n)
		for i := range n {
			positions[i] = i
		}
		return positions
	}
	positions := make([]int, maxFlips)
	for i := range maxFlips {
		positions[i] = i * (n - 1) / (maxFlips - 1)
	}
	return positions
}

// foreignAddr — адрес вне любой сети отправителя, содержащей addr: старший бит
// перевёрнут.
func foreignAddr(addr netip.Addr) netip.Addr {
	if addr.Is4() {
		b := addr.As4()
		b[0] ^= 0x80
		return netip.AddrFrom4(b)
	}
	b := addr.As16()
	b[0] ^= 0x80
	return netip.AddrFrom16(b)
}

func withHeader(req inbox.Request, name string, values []string) inbox.Request {
	out := cloneRequest(req)
	if values == nil {
		delete(out.Headers, name)
		return out
	}
	out.Headers[name] = values
	return out
}

func withRemote(req inbox.Request, remote string) inbox.Request {
	out := cloneRequest(req)
	out.RemoteIP = remote
	return out
}

// cloneRequest — копия до последнего среза.
func cloneRequest(req inbox.Request) inbox.Request {
	out := inbox.Request{Raw: bytes.Clone(req.Raw), RemoteIP: req.RemoteIP}
	if req.Headers != nil {
		out.Headers = make(map[string][]string, len(req.Headers))
		for name, values := range req.Headers {
			out.Headers[name] = slices.Clone(values)
		}
	}
	return out
}

func sameRequest(a, b inbox.Request) bool {
	return bytes.Equal(a.Raw, b.Raw) && a.RemoteIP == b.RemoteIP &&
		maps.EqualFunc(a.Headers, b.Headers, slices.Equal[[]string])
}
