package netns

import (
	"net/netip"
	"testing"
)

// The checks on a delegated prefix name the subnets and the hosts inside it the way a
// declaration does: a subnet ID and a token. Working out which address that is, is pure
// arithmetic, and it is worked out here independently of any implementation under test.
// Getting it wrong would make every check against the delegation look at the wrong
// address, so it is pinned down outside the build tag.
func TestDelegatedSubnet(t *testing.T) {
	tests := []struct {
		name       string
		delegation string
		subnetID   uint64
		want       string
		wantErr    bool
	}{
		{"the first subnet of a /56", "2001:db8:100::/56", 1, "2001:db8:100:1::/64", false},
		{"subnet zero is the delegation's own first /64", "2001:db8:100::/56", 0, "2001:db8:100::/64", false},
		{"the last subnet of a /56", "2001:db8:100::/56", 0xff, "2001:db8:100:ff::/64", false},
		{"a /56 that does not start on a group boundary", "2001:db8:1:ab00::/56", 2, "2001:db8:1:ab02::/64", false},
		{"a /48 has sixteen bits of subnet", "2001:db8:1::/48", 0x1234, "2001:db8:1:1234::/64", false},
		{"a subnet ID that does not fit in a /56", "2001:db8:100::/56", 0x100, "", true},
		{"a delegation longer than /64 has no subnets", "2001:db8:100::/72", 0, "", true},
		{"an IPv4 prefix is not a delegation", "192.0.2.0/24", 1, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := delegatedSubnet(netip.MustParsePrefix(tc.delegation), tc.subnetID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := netip.MustParsePrefix(tc.want); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func TestDelegatedAddress(t *testing.T) {
	tests := []struct {
		name       string
		delegation string
		subnetID   uint64
		token      string
		want       string
		wantErr    bool
	}{
		{"the router's address in the first subnet", "2001:db8:100::/56", 1, "::1", "2001:db8:100:1::1", false},
		{"a host's token", "2001:db8:200::/56", 1, "::30", "2001:db8:200:1::30", false},
		{"a token filling the whole host part", "2001:db8:100::/56", 2, "::1:2:3:4", "2001:db8:100:2:1:2:3:4", false},
		{"a token reaching into the subnet bits", "2001:db8:100::/56", 1, "0:0:0:1::1", "", true},
		{"a token that is not an address", "2001:db8:100::/56", 1, "host", "", true},
		{"a subnet ID that does not fit", "2001:db8:100::/56", 0x100, "::1", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := delegatedAddress(netip.MustParsePrefix(tc.delegation), tc.subnetID, tc.token)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := netip.MustParseAddr(tc.want); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

// A LAN host answers an inbound IPv6 connection with the address it was reached at, in
// the form socat gives it: bracketed and fully expanded. Comparing that text with an
// address as Go prints it would never match, so it is read as an address.
func TestParseAnsweredAddress(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{"bracketed and expanded, as socat prints it", "[2001:0db8:0100:0001:0000:0000:0000:0030]\n", "2001:db8:100:1::30", false},
		{"already compressed", "2001:db8:100:1::30", "2001:db8:100:1::30", false},
		{"the same answer twice", "[2001:db8:100:2::1]\n[2001:db8:100:2::1]\n", "2001:db8:100:2::1", false},
		{"two different answers", "[2001:db8:100:2::1]\n[2001:db8:100:1::30]\n", "", true},
		{"nothing", "", "", true},
		{"not an address", "sshd-stub 192.0.2.1", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAnsweredAddress(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := netip.MustParseAddr(tc.want); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}
