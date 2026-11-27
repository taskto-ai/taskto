package egress

import "net/netip"

var specialRanges = []struct {
	prefix netip.Prefix
	kind   string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "reserved"},
	{netip.MustParsePrefix("10.0.0.0/8"), "private"},
	{netip.MustParsePrefix("100.64.0.0/10"), "CGNAT"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local"},
	{netip.MustParsePrefix("168.63.129.16/32"), "cloud metadata"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private"},
	{netip.MustParsePrefix("192.0.0.0/24"), "reserved"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation"},
	{netip.MustParsePrefix("192.88.99.0/24"), "reserved"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private"},
	{netip.MustParsePrefix("198.18.0.0/15"), "reserved"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},

	{netip.MustParsePrefix("::/128"), "reserved"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "private"},
	{netip.MustParsePrefix("100::/64"), "reserved"},
	{netip.MustParsePrefix("2001::/23"), "reserved"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("3fff::/20"), "documentation"},
	{netip.MustParsePrefix("fc00::/7"), "unique-local"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

var (
	nat64   = netip.MustParsePrefix("64:ff9b::/96")
	sixTo4  = netip.MustParsePrefix("2002::/16")
	global6 = netip.MustParsePrefix("2000::/3")
)

func Special(addr netip.Addr) string {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return "invalid"
	}
	if addr.Is6() {
		b := addr.As16()
		switch {
		case nat64.Contains(addr):
			return Special(netip.AddrFrom4([4]byte(b[12:16])))
		case sixTo4.Contains(addr):
			return Special(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	for _, r := range specialRanges {
		if r.prefix.Contains(addr) {
			return r.kind
		}
	}
	if addr.Is6() && !global6.Contains(addr) {
		return "reserved"
	}
	return ""
}
