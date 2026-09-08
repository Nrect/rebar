package ratelimithttp

import (
	"net/http"
	"net/netip"
	"strings"
)

// forwardedHeader — цепочка прокси; читается только от доверенного соседа.
const forwardedHeader = "X-Forwarded-For"

// ClientIP — адрес клиента как строка ("" если разобрать не удалось).
//
// Сосед по соединению не из trusted — возвращается только он: заголовку от
// произвольного клиента верить нельзя. От доверенного прокси цепочка
// читается справа налево до первого недоверенного адреса; мусор в цепочке
// обрывает разбор там же.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := peerAddr(r.RemoteAddr)
	if !ok {
		return ""
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}

	client := peer
	// Заголовок разбирается только за доверенным прокси, поэтому длину
	// цепочки задаёт он, а не произвольный клиент.
	hops := forwardedFor(r)
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		client = normalize(addr)
		if !isTrusted(client, trusted) {
			break
		}
	}
	return client.String()
}

// ByIP — ключ лимита по адресу клиента. IPv6 сворачивается в префикс
// v6PrefixBits (обычно 64): у клиента их 2^64, и полный адрес дал бы каждому
// свой лимит.
func ByIP(trusted []netip.Prefix, v6PrefixBits int) KeyFunc {
	if v6PrefixBits < 1 || v6PrefixBits > 128 {
		panic("ratelimithttp.ByIP: v6PrefixBits must be in [1, 128]")
	}
	return func(r *http.Request) string {
		addr, err := netip.ParseAddr(ClientIP(r, trusted))
		if err != nil {
			return ""
		}
		if addr.Is4() {
			return addr.String()
		}
		prefix, err := addr.Prefix(v6PrefixBits)
		if err != nil {
			return ""
		}
		return prefix.String()
	}
}

// peerAddr — адрес соседа по соединению; порт в RemoteAddr необязателен.
func peerAddr(remote string) (netip.Addr, bool) {
	if hostPort, err := netip.ParseAddrPort(remote); err == nil {
		return normalize(hostPort.Addr()), true
	}
	addr, err := netip.ParseAddr(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return normalize(addr), true
}

// normalize — IPv4 в форме ::ffff:a.b.c.d и адрес с зоной иначе не совпали бы
// ни с одним префиксом trusted.
func normalize(addr netip.Addr) netip.Addr { return addr.Unmap().WithZone("") }

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedFor — записи цепочки по порядку; заголовок может прийти
// несколькими строками.
func forwardedFor(r *http.Request) []string {
	values := r.Header.Values(forwardedHeader)
	hops := make([]string, 0, len(values))
	for _, value := range values {
		for entry := range strings.SplitSeq(value, ",") {
			hops = append(hops, strings.TrimSpace(entry))
		}
	}
	return hops
}
