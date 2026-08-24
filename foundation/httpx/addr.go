package httpx

import "net/netip"

// ParseAddr extracts a bare address from "ip" or the host part of "ip:port";
// anything unparseable returns nil, which is what a nullable inet column wants.
//
// Both forms are accepted on purpose: the audit writer receives whatever the
// request layer produced, and behind some proxies that is "ip:port". Two copies
// of this used to disagree exactly on that case -- one dropped such rows to
// NULL (ADR-0206).
func ParseAddr(ip string) *netip.Addr {
	if ip == "" {
		return nil
	}
	if addr, err := netip.ParseAddr(ip); err == nil {
		return &addr
	}
	if ap, err := netip.ParseAddrPort(ip); err == nil {
		a := ap.Addr()
		return &a
	}
	return nil
}
