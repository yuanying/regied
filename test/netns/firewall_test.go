//go:build netns

package netns

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Check 6. Inbound traffic that is not allowed gets dropped.
//
// "It does not connect" alone cannot tell whether the firewall dropped the packet or
// nothing was listening and an RST came back. If it is being dropped, the connection
// times out; if it is not, it is refused immediately. What this observes is that
// difference.
func TestFirewallDropsUnsolicitedInbound(t *testing.T) {
	// Confirm first that the allowed way in (the port forward) works. Without that, the
	// failure below would just as well be the flip side of "nothing reaches us from
	// outside in the first place".
	eventuallyPeer(t, "connectivity through the allowed way in", func() (peerAddr, error) {
		if _, err := dialStub(t, nsInternet, internetA, pppoeGlobalIP, forwardWANPort); err != nil {
			return peerAddr{}, err
		}
		return peerAddr{IP: pppoeGlobalIP, Port: forwardWANPort}, nil
	})

	const connectTimeout = 4 * time.Second

	out, elapsed, err := nsExec(t, nsInternet, cmdTimeout, "",
		"socat", "-t", "3", "-T", "10", "STDIO",
		fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=%d",
			pppoeGlobalIP, blockedWANPort, internetA, int(connectTimeout.Seconds())))

	if err == nil {
		t.Fatalf("a connection to %s:%d, which is not allowed, went through: the response was %q",
			pppoeGlobalIP, blockedWANPort, strings.TrimSpace(out))
	}
	if strings.Contains(err.Error(), "refused") {
		t.Fatalf("a connection to %s:%d, which is not allowed, came back refused (%v). "+
			"Returning an RST means the packet reached the router and was processed, so "+
			"the filter did not drop it", pppoeGlobalIP, blockedWANPort, err)
	}
	if elapsed < connectTimeout/2 {
		t.Fatalf("a connection to %s:%d, which is not allowed, failed after %s. If it were "+
			"being dropped the connection attempt would time out (around %s), so this is "+
			"too early: %v", pppoeGlobalIP, blockedWANPort, elapsed, connectTimeout, err)
	}
}
