// The arithmetic of a delegated prefix lives outside the build tag, like reading a
// response does: it needs neither a network namespace nor privileges, and a mistake in it
// would point every check against the delegation at the wrong address.

package netns

import (
	"fmt"
	"net/netip"
	"strings"
)

// delegatedSubnet returns the /64 a subnet ID names inside a delegated prefix. It is what
// fromDelegatedPrefix without a token means, worked out here without asking the
// implementation under test.
func delegatedSubnet(delegation netip.Prefix, subnetID uint64) (netip.Prefix, error) {
	if !delegation.Addr().Is6() || delegation.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%s is not an IPv6 prefix", delegation)
	}
	bits := 64 - delegation.Bits()
	if bits < 0 {
		return netip.Prefix{}, fmt.Errorf("%s is longer than /64 and has no subnets", delegation)
	}
	if bits < 64 && subnetID>>bits != 0 {
		return netip.Prefix{}, fmt.Errorf("subnet ID %#x does not fit in %s", subnetID, delegation)
	}

	b := delegation.Masked().Addr().As16()
	for i := 7; i >= 0 && subnetID != 0; i-- {
		b[i] |= byte(subnetID)
		subnetID >>= 8
	}
	return netip.PrefixFrom(netip.AddrFrom16(b), 64), nil
}

// delegatedAddress returns the address a subnet ID and a token name inside a delegated
// prefix: what fromDelegatedPrefix with a token means.
func delegatedAddress(delegation netip.Prefix, subnetID uint64, token string) (netip.Addr, error) {
	subnet, err := delegatedSubnet(delegation, subnetID)
	if err != nil {
		return netip.Addr{}, err
	}
	t, err := netip.ParseAddr(token)
	if err != nil || !t.Is6() {
		return netip.Addr{}, fmt.Errorf("token %q is not an IPv6 interface identifier", token)
	}

	b, host := subnet.Addr().As16(), t.As16()
	for i := range 8 {
		if host[i] != 0 {
			return netip.Addr{}, fmt.Errorf("token %s reaches into the upper 64 bits", token)
		}
	}
	copy(b[8:], host[8:])
	return netip.AddrFrom16(b), nil
}

// parseAnsweredAddress reads the answer of a LAN host reached over IPv6: the address it
// was reached at. socat prints it bracketed and fully expanded. As with parsePeer,
// identical answers side by side count as one, and answers that disagree are an error.
func parseAnsweredAddress(out string) (netip.Addr, error) {
	var got netip.Addr
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
		if err != nil {
			return netip.Addr{}, fmt.Errorf("the answer %q is not an address", line)
		}
		if got.IsValid() && got != addr {
			return netip.Addr{}, fmt.Errorf("the answers disagree: %s and %s", got, addr)
		}
		got = addr
	}
	if !got.IsValid() {
		return netip.Addr{}, fmt.Errorf("no answer came back")
	}
	return got, nil
}
