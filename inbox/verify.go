package inbox

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// Verifier — подлинность доставки: порт, который пишет проект при подключении
// отправителя. Контракт — ADR-0012, решение 3; держит inboxtest.RunVerifierSuite.
// Отказы: не прошло проверку — ErrNotAuthentic, подлинное, но непригодное —
// ErrMalformed, проверить не смогли — ErrUnavailable. Тело и подпись в текст
// ошибки не кладутся: он уходит в лог.
type Verifier interface {
	Verify(ctx context.Context, req Request) (Event, error)
}

// CheckTimestamp — подписанный момент не дальше tolerance от now в обе
// стороны; иначе ErrNotAuthentic.
//
// ДОПУСК НЕ НОЛЬ: у Stripe нулевой допуск выключает проверку свежести целиком,
// поэтому tolerance <= 0 — паника, а не «без проверки».
func CheckTimestamp(signed, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		panic("inbox.CheckTimestamp: tolerance must be positive: zero would disable the freshness check")
	}
	if age := now.Sub(signed); age > tolerance || age < -tolerance {
		return fmt.Errorf("%w: signed timestamp is outside the tolerance", ErrNotAuthentic)
	}
	return nil
}

// AddrIn — адрес от периметра входит в одну из сетей отправителя. IPv4 внутри
// IPv6 и зона приводятся, как в ratelimithttp.ClientIP: иначе адрес не совпал
// бы ни с одной сетью. Пустой, негодный или с портом — false: проверять нечем
// значит отказ.
func AddrIn(remote string, nets []netip.Prefix) bool {
	addr, err := netip.ParseAddr(remote)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, network := range nets {
		if network.Contains(addr) {
			return true
		}
	}
	return false
}
