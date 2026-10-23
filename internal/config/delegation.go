package config

import (
	"cmp"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
)

// DelegatedSubnetBits is the length of every subnet cut from a delegation. A delegated
// subnet is handed out as a /64, and a field for another length waits for a
// configuration that needs one (ADR 0021).
const DelegatedSubnetBits = 64

// Delegations is the prefix each prefix-delegation client currently holds, by the name
// of the Interface running it. It is a runtime value: a turn reads it from networkd's
// description of the link, and it may be missing now and present on the next turn
// (ADR 0021).
//
// A client with no entry, or with an empty one, holds nothing yet. More than one prefix
// is kept as it was read, so that resolving against it can say so instead of picking one.
type Delegations map[string][]netip.Prefix

// Resolved is one address resolved from a delegation, and the delegation it was cut
// from, which is what a dry-run shows beside it.
type Resolved struct {
	Address    netip.Addr
	Delegation netip.Prefix
}

// DelegationError says why a value derived from the delegated prefix cannot be resolved
// this turn. It is not a failure of the turn: what depends on the value waits for it,
// and the message is written as what it waits for (ADR 0021).
type DelegationError struct{ Waiting string }

func (e *DelegationError) Error() string { return e.Waiting }

// Address is the address a fromDelegatedPrefix with a token names: the delegated
// prefix, the subnet ID in the bits between its length and 64, and the token in the
// host half. It is the arithmetic networkd does for [DHCPPrefixDelegation] with
// SubnetId= and Token=static:, so the address resolved here is the one networkd put on
// the link.
func (d Delegations) Address(derived v1alpha1.DelegatedPrefixAddr) (Resolved, error) {
	subnet, err := d.Subnet(derived)
	if err != nil {
		return Resolved{}, err
	}
	token, err := netip.ParseAddr(derived.Token)
	if err != nil || !token.Is6() {
		// Validation refuses a token that is not an interface identifier.
		return Resolved{}, fmt.Errorf("the token %q is not an interface identifier", derived.Token)
	}
	bytes := subnet.Addr().As16()
	host := token.As16()
	copy(bytes[8:], host[8:])
	delegation, _ := d.delegation(derived.InterfaceRef)
	return Resolved{Address: netip.AddrFrom16(bytes), Delegation: delegation}, nil
}

// Subnet is the /64 a fromDelegatedPrefix names, its token aside.
func (d Delegations) Subnet(derived v1alpha1.DelegatedPrefixAddr) (netip.Prefix, error) {
	delegation, err := d.delegation(derived.InterfaceRef)
	if err != nil {
		return netip.Prefix{}, err
	}
	subnetID := 0
	if derived.SubnetID != nil {
		subnetID = *derived.SubnetID
	}
	bits := DelegatedSubnetBits - delegation.Bits()
	if bits < 0 {
		return netip.Prefix{}, &DelegationError{Waiting: fmt.Sprintf(
			"a delegated prefix on Interface/%s that holds a /%d; it holds %s, which is longer",
			derived.InterfaceRef, DelegatedSubnetBits, delegation)}
	}
	// A subnet ID that does not fit is not a validation error: how long the delegation
	// is, is not known until the provider has handed it out (ADR 0021).
	if subnetID < 0 || bits < 63 && uint64(subnetID) >= uint64(1)<<bits {
		return netip.Prefix{}, &DelegationError{Waiting: fmt.Sprintf(
			"a delegated prefix on Interface/%s with room for subnetID %d; it holds %s, which has %d subnets",
			derived.InterfaceRef, subnetID, delegation, uint64(1)<<bits)}
	}
	bytes := delegation.Addr().As16()
	upper := uint64(0)
	for _, b := range bytes[:8] {
		upper = upper<<8 | uint64(b)
	}
	upper |= uint64(subnetID)
	for i := 7; i >= 0; i-- {
		bytes[i] = byte(upper)
		upper >>= 8
	}
	return netip.PrefixFrom(netip.AddrFrom16(bytes), DelegatedSubnetBits), nil
}

// delegation is the one prefix an interface's client holds. None is a reason to wait;
// more than one is too, because which of them a value is cut from is not stated.
func (d Delegations) delegation(name string) (netip.Prefix, error) {
	prefixes := d[name]
	switch len(prefixes) {
	case 0:
		return netip.Prefix{}, &DelegationError{Waiting: "a delegated prefix on Interface/" + name}
	case 1:
		return prefixes[0].Masked(), nil
	}
	held := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		held[i] = prefix.String()
	}
	return netip.Prefix{}, &DelegationError{Waiting: fmt.Sprintf(
		"Interface/%s to hold one delegated prefix; it holds %d (%s)", name, len(prefixes), strings.Join(held, ", "))}
}

// AdvertisedAddress is one address networkd configured on a link from a router
// advertisement, and how long networkd prefers it, as networkd describes the link.
//
// PreferredUntil is networkd's own figure, compared only with another address's on the
// same host; an address networkd describes with no preferred lifetime is preferred for
// ever, and carries the largest value there is.
type AdvertisedAddress struct {
	Address        netip.Addr
	PreferredUntil uint64
}

// DelegationFromAdvertisement is the delegation a downstream host knows from the
// addresses its LAN took from advertisements: the /64 of the address carrying the token,
// cut back to the declared length (ADR 0023).
//
// Only a global address counts. A unique local address advertised beside it is not the
// delegation, and a temporary address does not carry the token. Two subnets of one
// delegation are one delegation.
//
// held is what Delegations holds for the interface. None is a reason to wait. More than
// one delegation is ordinary for a while after a prefix change, and the one preferred
// longest is taken — the new one, as soon as it has arrived — with the others in
// passedOver for the turn to report. Delegations preferred equally long cannot be told
// apart, and are all held, which makes everything cut from them wait and say why.
func DelegationFromAdvertisement(addresses []AdvertisedAddress, token string, length int) (held, passedOver []netip.Prefix) {
	preferred := make(map[netip.Prefix]uint64)
	for _, advertised := range withToken(addresses, token) {
		delegation, err := advertised.Address.Prefix(length)
		if err != nil {
			continue
		}
		if until, seen := preferred[delegation]; !seen || advertised.PreferredUntil > until {
			preferred[delegation] = advertised.PreferredUntil
		}
	}
	return preferredLongest(preferred, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
}

// AddressFromAdvertisement is the address a link took from the advertisement with the
// declared token, which is what an AAAA record naming the link holds (ADR 0024). It is
// chosen as DelegationFromAdvertisement chooses the delegation: the one preferred longest
// is held, the others are passed over, and addresses preferred equally long are all held.
func AddressFromAdvertisement(addresses []AdvertisedAddress, token string) (held, passedOver []netip.Addr) {
	preferred := make(map[netip.Addr]uint64)
	for _, advertised := range withToken(addresses, token) {
		if until, seen := preferred[advertised.Address]; !seen || advertised.PreferredUntil > until {
			preferred[advertised.Address] = advertised.PreferredUntil
		}
	}
	return preferredLongest(preferred, netip.Addr.Compare)
}

// withToken is the global addresses carrying the token. A unique local address
// advertised beside the global prefix is not the delegation, and a temporary address
// does not carry the token.
func withToken(addresses []AdvertisedAddress, token string) []AdvertisedAddress {
	identifier, err := netip.ParseAddr(token)
	if err != nil || !identifier.Is6() {
		return nil
	}
	host := identifier.As16()
	var out []AdvertisedAddress
	for _, advertised := range addresses {
		address := advertised.Address
		if !address.Is6() || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() {
			continue
		}
		if bytes := address.As16(); [8]byte(bytes[8:]) != [8]byte(host[8:]) {
			continue
		}
		out = append(out, advertised)
	}
	return out
}

// preferredLongest orders candidates by how long each is preferred. The one preferred
// longest is held and the rest passed over; when the first two are preferred equally long
// none can be told from the others, and all are held, in address order.
func preferredLongest[K comparable](preferred map[K]uint64, compare func(a, b K) int) (held, passedOver []K) {
	candidates := slices.Collect(maps.Keys(preferred))
	slices.SortFunc(candidates, func(a, b K) int {
		if order := cmp.Compare(preferred[b], preferred[a]); order != 0 {
			return order
		}
		return compare(a, b)
	})
	switch {
	case len(candidates) <= 1:
		return candidates, nil
	case preferred[candidates[0]] == preferred[candidates[1]]:
		slices.SortFunc(candidates, compare)
		return candidates, nil
	}
	return candidates[:1], candidates[1:]
}

// Advertised is the address each interface took from the router advertisement with its
// token, by the name of the Interface. Like Delegations it is a runtime value, read every
// turn, and more than one address is kept as it was read so that resolving against it
// can say so (ADR 0024).
type Advertised map[string][]netip.Addr

// Address is the one address an interface took from the advertisement. None is a reason
// to wait; more than one is too, because which of them the record should hold cannot be
// told.
func (a Advertised) Address(name string) (netip.Addr, error) {
	addresses := a[name]
	switch len(addresses) {
	case 0:
		return netip.Addr{}, &DelegationError{Waiting: "an address on Interface/" + name + " taken from the router advertisement"}
	case 1:
		return addresses[0], nil
	}
	held := make([]string, len(addresses))
	for i, address := range addresses {
		held[i] = address.String()
	}
	return netip.Addr{}, &DelegationError{Waiting: fmt.Sprintf(
		"Interface/%s to hold one address taken from the router advertisement; it holds %d preferred equally long (%s)",
		name, len(addresses), strings.Join(held, ", "))}
}
