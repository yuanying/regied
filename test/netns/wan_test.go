//go:build netns

package netns

import "testing"

// Check 1. A host in the PPPoE range can reach the outside. This goes as far as
// confirming that the source seen from outside is the global address handed out over
// PPPoE. What is being observed is not merely "it connects" but "it went over PPPoE".
func TestOutboundViaPPPoE(t *testing.T) {
	got := eventuallyPeer(t, "outbound connectivity over PPPoE", func() (peerAddr, error) {
		return tcpWhoami(t, clientPPPoESrc, internetA)
	})

	if got.IP != pppoeGlobalIP {
		t.Fatalf("traffic from %s did not go over PPPoE: the source seen from outside was %s, expected %s",
			clientPPPoESrc, got.IP, pppoeGlobalIP)
	}
}

// Check 2. A host on the DS-Lite (ipip6 tunnel) side can reach the outside. If it comes
// out under the address the AFTR uses for NAT44, it went through the tunnel.
func TestOutboundViaDSLite(t *testing.T) {
	got := eventuallyPeer(t, "outbound connectivity over DS-Lite", func() (peerAddr, error) {
		return tcpWhoami(t, clientDSLiteSrc, internetA)
	})

	if got.IP != dsliteGlobalIP {
		t.Fatalf("traffic from %s did not go over DS-Lite: the source seen from outside was %s, expected %s",
			clientDSLiteSrc, got.IP, dsliteGlobalIP)
	}
}

// Check 3. Hosts on the same LAN leave through different exits depending on the source
// range. 192.168.1.10-99 goes over PPPoE, anything else over DS-Lite (the default).
func TestPolicyRoutingSplitsBySourceRange(t *testing.T) {
	cases := []struct {
		name   string
		srcIP  string
		wantIP string
	}{
		{"toward the low end of the PPPoE range", clientPPPoESrc, pppoeGlobalIP},
		{"the port forward's destination, inside the PPPoE range", lanServerIP, pppoeGlobalIP},
		{"outside the range, so the default DS-Lite", clientDSLiteSrc, dsliteGlobalIP},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		got := eventuallyPeer(t, "outbound connectivity for "+tc.name, func() (peerAddr, error) {
			return tcpWhoami(t, tc.srcIP, internetA)
		})
		seen[tc.srcIP] = got.IP

		if got.IP != tc.wantIP {
			t.Errorf("%s (%s) left through the wrong exit: the source seen from outside was %s, expected %s",
				tc.name, tc.srcIP, got.IP, tc.wantIP)
		}
	}

	// If everything leaves through the same exit, the split is not working at all.
	if seen[clientPPPoESrc] == seen[clientDSLiteSrc] {
		t.Fatalf("the source range did not split the paths: both %s and %s leave from %s",
			clientPPPoESrc, clientDSLiteSrc, seen[clientPPPoESrc])
	}
}
