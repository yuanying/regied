package netns

import "testing"

// Whether the router asked the DHCPv6 server for anything is read off a counter on the
// provider side. Misreading it would make a router that solicited look quiet, so reading
// it is pinned down outside the build tag.
func TestParseCounterPackets(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{"as nft lists it", "table ip6 dhcpv6 {\n\tcounter requests {\n\t\tpackets 12 bytes 1440\n\t}\n}\n", 12, false},
		{"nothing counted yet", "table ip6 dhcpv6 {\n\tcounter requests {\n\t\tpackets 0 bytes 0\n\t}\n}\n", 0, false},
		{"no counter", "", 0, true},
		{"a count that is not a number", "counter requests {\n\tpackets many bytes 0\n}\n", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCounterPackets(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
