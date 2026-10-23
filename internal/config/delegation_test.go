package config_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

func derivation(subnetID int, token string) v1alpha1.DelegatedPrefixAddr {
	return v1alpha1.DelegatedPrefixAddr{InterfaceRef: "wan", SubnetID: &subnetID, Token: token}
}

// An address inside the delegation is the delegated prefix, the subnet ID in the bits
// between its length and 64, and the token in the host half. It is the same arithmetic
// networkd does for [DHCPPrefixDelegation] with SubnetId= and Token=static:, so a value
// resolved here is the address networkd put on the link (ADR 0021).
func TestDelegatedAddressIsThePrefixTheSubnetAndTheToken(t *testing.T) {
	delegations := config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/56")}}

	cases := []struct {
		subnetID int
		token    string
		want     string
	}{
		{1, "::1", "2001:db8:1000:1::1"},
		{0, "::1", "2001:db8:1000::1"},
		{255, "::1", "2001:db8:1000:ff::1"},
		{0x10, "::10", "2001:db8:1000:10::10"},
		{2, "::1:2:3:4", "2001:db8:1000:2:1:2:3:4"},
	}
	for _, c := range cases {
		resolved, err := delegations.Address(derivation(c.subnetID, c.token))
		if err != nil {
			t.Errorf("subnetID %d token %s: %v", c.subnetID, c.token, err)
			continue
		}
		if resolved.Address.String() != c.want {
			t.Errorf("subnetID %d token %s is %s, want %s", c.subnetID, c.token, resolved.Address, c.want)
		}
		if resolved.Delegation.String() != "2001:db8:1000::/56" {
			t.Errorf("the resolution says it came from %s, want the delegation it was cut from", resolved.Delegation)
		}
	}
}

// A subnet is the same arithmetic without the token, as the /64 a delegated subnet is
// handed out as.
func TestDelegatedSubnetIsASlash64(t *testing.T) {
	delegations := config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/56")}}
	subnet, err := delegations.Subnet(derivation(3, ""))
	if err != nil {
		t.Fatal(err)
	}
	if subnet.String() != "2001:db8:1000:3::/64" {
		t.Errorf("subnet is %s, want 2001:db8:1000:3::/64", subnet)
	}
}

// What the turn could not resolve is not a failure, only a reason to wait, and the
// reason has to say what is missing (ADR 0021).
func TestADelegatedValueThatCannotBeResolvedSaysWhy(t *testing.T) {
	cases := []struct {
		name        string
		delegations config.Delegations
		subnetID    int
		want        []string
	}{
		{
			name:        "no delegation",
			delegations: config.Delegations{},
			subnetID:    1,
			want:        []string{"a delegated prefix", "Interface/wan"},
		},
		{
			name:        "an empty delegation",
			delegations: config.Delegations{"wan": nil},
			subnetID:    1,
			want:        []string{"a delegated prefix", "Interface/wan"},
		},
		{
			name: "two delegations",
			delegations: config.Delegations{"wan": {
				netip.MustParsePrefix("2001:db8:1000::/56"),
				netip.MustParsePrefix("2001:db8:2000::/56"),
			}},
			subnetID: 1,
			want:     []string{"Interface/wan", "2001:db8:1000::/56", "2001:db8:2000::/56"},
		},
		{
			name:        "a subnet ID beyond the delegation",
			delegations: config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/56")}},
			subnetID:    256,
			want:        []string{"subnetID 256", "2001:db8:1000::/56"},
		},
		{
			name:        "a /64 has only subnet 0",
			delegations: config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/64")}},
			subnetID:    1,
			want:        []string{"subnetID 1", "2001:db8:1000::/64"},
		},
		{
			name:        "a delegation longer than a subnet",
			delegations: config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/72")}},
			subnetID:    0,
			want:        []string{"2001:db8:1000::/72"},
		},
	}
	for _, c := range cases {
		_, err := c.delegations.Address(derivation(c.subnetID, "::1"))
		if err == nil {
			t.Errorf("%s: resolved, want a reason it cannot be", c.name)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the reason %q does not mention %q", c.name, err, want)
			}
		}
	}
}

// A /64 delegation still has subnet 0, which is the whole of it.
func TestASlash64DelegationHasSubnetZero(t *testing.T) {
	delegations := config.Delegations{"wan": {netip.MustParsePrefix("2001:db8:1000::/64")}}
	resolved, err := delegations.Address(derivation(0, "::1"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Address.String() != "2001:db8:1000::1" {
		t.Errorf("address is %s, want 2001:db8:1000::1", resolved.Address)
	}
}
