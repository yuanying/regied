package config

import (
	"fmt"
	"net/netip"
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
