//go:build netns

package netns

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The delegation side of the topology. These are paired with hack/netns/lib.sh. Do not
// change one side without the other.
const (
	// The two prefixes the DHCPv6 server delegates, one at a time. Moving from the first to
	// the second is the prefix change the checks below reproduce.
	firstDelegation  = "2001:db8:100::/56"
	secondDelegation = "2001:db8:200::/56"

	// Where the device under test puts things inside the delegation. Each is what a
	// declaration would write as fromDelegatedPrefix.
	lanSubnetID        = 1      // the LAN's /64
	routerLANToken     = "::1"  // the router's own address on it, and DS-Lite's local address
	publishedHostToken = "::30" // a LAN host the firewall lets the outside reach
	hiddenHostToken    = "::31" // a LAN host on the same /64 that it does not
	downstreamSubnetID = 2      // a /64 routed to the published host, as to a host behind it
	downstreamToken    = "::1"  // the address the host behind the router holds in it

	// The internet side's IPv6 address, the source of the inbound checks.
	internetV6 = "2001:db8:fe::10"

	// The port a LAN host answers on over IPv6. The answer is the address it was reached
	// at, so a reply also says which of the host's addresses the router delivered to.
	inboundV6Port = 8030
)

// How long the delegation may take to move. The server hands out short lifetimes (see
// lib.sh), so a renewal brings a new prefix within seconds and a withdrawn one expires
// within half a minute; the rest is for the device under test to act on what it got.
const delegationTimeout = 90 * time.Second

// Check 10. Everything a router derives from its delegated prefix follows the prefix:
// the LAN's address, DS-Lite's local address, a route to a subnet handed to a host
// behind the router, and the set of hosts the firewall lets in. While nothing is
// delegated, what lives inside the prefix is gone and the rest of the router keeps
// working.
//
// What is observed is only what an outsider can see. The AFTR accepts the tunnel only
// from the address the delegation puts on the router's LAN, so IPv4 over DS-Lite works
// only if the tunnel's local address followed. The inbound checks reach hosts inside
// the prefix from the internet side, so they work only if the router routes to them and
// lets them through, and the hidden host shows the firewall is what decides.
//
// The phases build on each other and run in order. A phase that fails stops the rest,
// because each one starts from the state the previous one left.
func TestDelegatedPrefix(t *testing.T) {
	first := netip.MustParsePrefix(firstDelegation)
	second := netip.MustParsePrefix(secondDelegation)

	// The topology comes up delegating the first prefix.
	if !t.Run("values are derived from the delegation", func(t *testing.T) {
		requireDerived(t, first)
	}) {
		return
	}

	if !t.Run("values follow a new delegation", func(t *testing.T) {
		delegate(t, second)
		requireDerived(t, second)
	}) {
		return
	}

	if !t.Run("nothing inside the prefix is reachable while nothing is delegated", func(t *testing.T) {
		withdraw(t)
		published := mustDelegatedAddress(t, second, lanSubnetID, publishedHostToken)
		eventually(t, delegationTimeout, fmt.Sprintf("%s to become unreachable once the delegation expired", published),
			func() error {
				if out, err := dialV6(t, published, 3*time.Second); err == nil {
					return fmt.Errorf("still answered: %q", out)
				}
				return nil
			})

		// Losing the delegation is not losing the router.
		got := eventuallyPeer(t, "outbound connectivity over PPPoE without a delegation", func() (peerAddr, error) {
			return tcpWhoami(t, clientPPPoESrc, internetA)
		})
		if got.IP != pppoeGlobalIP {
			t.Fatalf("traffic from %s did not go over PPPoE while nothing was delegated: the source seen from outside was %s, expected %s",
				clientPPPoESrc, got.IP, pppoeGlobalIP)
		}
	}) {
		return
	}

	// This also leaves the topology delegating the first prefix again, as it came up.
	t.Run("values are derived again once a delegation returns", func(t *testing.T) {
		delegate(t, first)
		requireDerived(t, first)
	})
}

// requireDerived checks, from outside, everything the device under test derives from the
// delegation.
func requireDerived(t *testing.T, delegation netip.Prefix) {
	t.Helper()

	got := eventuallyPeerWithin(t, delegationTimeout, "outbound connectivity over DS-Lite from "+delegation.String(),
		func() (peerAddr, error) { return tcpWhoami(t, clientDSLiteSrc, internetA) })
	if got.IP != dsliteGlobalIP {
		t.Fatalf("traffic from %s did not go over DS-Lite: the source seen from outside was %s, expected %s",
			clientDSLiteSrc, got.IP, dsliteGlobalIP)
	}

	published := mustDelegatedAddress(t, delegation, lanSubnetID, publishedHostToken)
	requireAnswers(t, published, "the published LAN host")

	downstream := mustDelegatedAddress(t, delegation, downstreamSubnetID, downstreamToken)
	requireAnswers(t, downstream, "the host behind the published one, through the route to its subnet")

	// The hidden host is on the same /64 as the published one, so the router has a route
	// to it. A connection that times out says the firewall dropped it; one refused at once
	// would say it reached somewhere that answered.
	hidden := mustDelegatedAddress(t, delegation, lanSubnetID, hiddenHostToken)
	const connectTimeout = 4 * time.Second
	out, elapsed, err := dialV6Timed(t, hidden, connectTimeout)
	if err == nil {
		t.Fatalf("a connection to %s, which the firewall does not let in, went through: the response was %q", hidden, out)
	}
	if strings.Contains(err.Error(), "refused") || elapsed < connectTimeout/2 {
		t.Fatalf("a connection to %s, which the firewall does not let in, failed after %s instead of timing out (%v). "+
			"It was not dropped", hidden, elapsed, err)
	}
}

// requireAnswers waits until a LAN host answers from the internet side at the given
// address, and checks that the answer came from that address.
func requireAnswers(t *testing.T, addr netip.Addr, what string) {
	t.Helper()
	var out string
	eventually(t, delegationTimeout, fmt.Sprintf("%s (%s) to answer from outside", what, addr), func() error {
		var err error
		out, err = dialV6(t, addr, 5*time.Second)
		return err
	})
	got, err := parseAnsweredAddress(out)
	if err != nil {
		t.Fatalf("connecting to %s, %s, got an answer that cannot be read: %v", addr, what, err)
	}
	if got != addr {
		t.Fatalf("connecting to %s, %s, was answered from %s", addr, what, got)
	}
}

// dialV6 connects from the internet side to a LAN host's IPv6 address and returns the
// line it answers with.
func dialV6(tb testing.TB, addr netip.Addr, connectTimeout time.Duration) (string, error) {
	tb.Helper()
	out, _, err := dialV6Timed(tb, addr, connectTimeout)
	return out, err
}

func dialV6Timed(tb testing.TB, addr netip.Addr, connectTimeout time.Duration) (string, time.Duration, error) {
	tb.Helper()
	return nsExec(tb, nsInternet, cmdTimeout, "",
		"socat", "-t", "3", "-T", "10", "STDIO",
		fmt.Sprintf("TCP6:[%s]:%d,bind=[%s],connect-timeout=%d",
			addr, inboundV6Port, internetV6, int(connectTimeout.Seconds())))
}

func mustDelegatedAddress(tb testing.TB, delegation netip.Prefix, subnetID uint64, token string) netip.Addr {
	tb.Helper()
	addr, err := delegatedAddress(delegation, subnetID, token)
	if err != nil {
		tb.Fatal(err)
	}
	return addr
}

// delegate makes the DHCPv6 server delegate the given prefix instead of the one it does
// now. The device under test learns of it on its next renewal, as it would on a real
// line whose provider moved it.
func delegate(tb testing.TB, prefix netip.Prefix) {
	tb.Helper()
	if out, err := topoCommand("delegate", prefix.String()); err != nil {
		tb.Fatalf("could not make the server delegate %s: %v\n%s", prefix, err, out)
	}
}

// withdraw stops the DHCPv6 server. What it delegated expires on its own.
func withdraw(tb testing.TB) {
	tb.Helper()
	if out, err := topoCommand("withdraw"); err != nil {
		tb.Fatalf("could not stop the DHCPv6 server: %v\n%s", err, out)
	}
}

func topoCommand(args ...string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(root, "hack", "netns", "topo.sh"), args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// eventually retries a condition until it holds or the timeout passes.
func eventually(tb testing.TB, timeout time.Duration, what string, cond func() error) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := cond()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("waited %s for %s: %v", timeout, what, err)
		}
		time.Sleep(time.Second)
	}
}

// eventuallyPeerWithin is eventuallyPeer with a timeout of its own, for a path that only
// comes back once the delegation has moved.
func eventuallyPeerWithin(tb testing.TB, timeout time.Duration, what string, probe func() (peerAddr, error)) peerAddr {
	tb.Helper()
	var got peerAddr
	eventually(tb, timeout, what, func() error {
		var err error
		got, err = probe()
		return err
	})
	return got
}
