//go:build netns

package netns

import (
	"fmt"
	"strings"
	"testing"
)

// dialStub connects over TCP from the named netns and reads the one line the far side
// returns.
func dialStub(tb testing.TB, ns, srcIP, dstIP string, dstPort int) (string, error) {
	tb.Helper()

	target := fmt.Sprintf("TCP4:%s:%d,connect-timeout=5", dstIP, dstPort)
	if srcIP != "" {
		target = fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=5", dstIP, dstPort, srcIP)
	}
	out, _, err := nsExec(tb, ns, cmdTimeout, "", "socat", "-t", "3", "-T", "10", "STDIO", target)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Check 4. Traffic arriving from outside, addressed to the router's global address,
// reaches a host inside the LAN.
func TestPortForwardFromInternet(t *testing.T) {
	var banner string
	eventuallyPeer(t, "the port forward from outside", func() (peerAddr, error) {
		got, err := dialStub(t, nsInternet, internetA, pppoeGlobalIP, forwardWANPort)
		if err != nil {
			return peerAddr{}, err
		}
		banner = got
		return peerAddr{IP: internetA, Port: forwardWANPort}, nil
	})

	if !strings.HasPrefix(banner, sshStubBanner) {
		t.Fatalf("connecting to %s:%d did not land on %s:%d inside the LAN: the response was %q",
			pppoeGlobalIP, forwardWANPort, lanServerIP, forwardLANPort, banner)
	}
	// Forwarding does not rewrite the source. The host on the LAN side sees the outside
	// peer as it is.
	if !strings.Contains(banner, internetA) {
		t.Errorf("the peer seen from the LAN side is not the outside address: the response was %q, expected it to contain %s",
			banner, internetA)
	}
}

// Check 5. The same port forward also works when a host inside the LAN connects to its
// own global address (the hairpin case). For the return path to go back through the
// router, the source has to be rewritten to the router's LAN address.
func TestHairpinNAT(t *testing.T) {
	var banner string
	eventuallyPeer(t, "hairpin NAT", func() (peerAddr, error) {
		got, err := dialStub(t, nsClient, clientPPPoESrc, pppoeGlobalIP, forwardWANPort)
		if err != nil {
			return peerAddr{}, err
		}
		banner = got
		return peerAddr{IP: clientPPPoESrc, Port: forwardWANPort}, nil
	})

	if !strings.HasPrefix(banner, sshStubBanner) {
		t.Fatalf("connecting from the LAN to %s:%d did not land on %s:%d inside the LAN: the response was %q",
			pppoeGlobalIP, forwardWANPort, lanServerIP, forwardLANPort, banner)
	}
	if !strings.Contains(banner, routerLANIP) {
		t.Errorf("the hairpin source was not rewritten to the router's LAN address: "+
			"the response was %q, expected it to contain %s. Without that rewrite the return "+
			"path does not go through the router, and the connection either never "+
			"establishes or becomes asymmetric", banner, routerLANIP)
	}
}

// Check 7. The external port does not change when the destination does, which is
// endpoint-independent mapping. Things such as online play on game consoles depend on
// it.
func TestNATMappingIsEndpointIndependent(t *testing.T) {
	const srcPort = 40001

	first := eventuallyPeer(t, "UDP to the first destination", func() (peerAddr, error) {
		return udpWhoami(t, clientPPPoESrc, srcPort, internetA)
	})
	second := eventuallyPeer(t, "UDP to the second destination", func() (peerAddr, error) {
		return udpWhoami(t, clientPPPoESrc, srcPort, internetB)
	})

	// Pin down first that the NAT being measured is the router's. If the path has fallen
	// through to the DS-Lite side, what gets measured is the AFTR's NAT, and this would
	// pass without ever looking at the router's mapping.
	if first.IP != pppoeGlobalIP {
		t.Fatalf("UDP from %s did not go through the router's NAT: the source seen from outside was %s, expected %s",
			clientPPPoESrc, first.IP, pppoeGlobalIP)
	}
	if first.IP != second.IP {
		t.Fatalf("the external address differs per destination: %s toward %s, %s toward %s",
			first.IP, internetA, second.IP, internetB)
	}
	if first.Port != second.Port {
		t.Fatalf("the external port differs per destination, which is endpoint-dependent "+
			"mapping: %d toward %s, %d toward %s. The source port was %d in both cases",
			first.Port, internetA, second.Port, internetB, srcPort)
	}
}
