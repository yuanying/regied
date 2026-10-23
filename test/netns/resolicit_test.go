//go:build netns

package netns

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The provider side of a line moving to another router. These are paired with
// hack/netns/topo.sh. Do not change one side without the other.
const (
	nsWAN = "rg-wan"

	// The MAC addresses of the two access concentrators the session moves between.
	firstConcentrator  = "00:00:5e:00:53:01"
	secondConcentrator = "00:00:5e:00:53:02"
)

// How long a redial to the same concentrator is watched for a request it should not
// make. A router that solicits on a redial does so as the session comes up.
const quietPeriod = 10 * time.Second

// Check 11. When the PPPoE session comes back from another access concentrator, the
// line may have moved to another of the provider's routers, and the router asks for its
// delegation again instead of waiting for its lease to run out. When it comes back from
// the same one, it asks for nothing, and what it derives from the delegation stays.
//
// The server hands out lifetimes of hours for most of this, so that a router that did
// not ask again would hold the old prefix well past the end of the check. Whether it asked
// is read from the provider side: the server's DHCPv6 port counts what reaches it.
//
// The phases build on each other and run in order, and the last one leaves the topology
// as it came up: delegating the first prefix, with the short lifetimes.
func TestSessionFromAnotherConcentrator(t *testing.T) {
	first := netip.MustParsePrefix(firstDelegation)
	second := netip.MustParsePrefix(secondDelegation)

	// The topology came up with a concentrator of no particular address. Moving to a
	// known one is a move like any other, to a router that kept the binding: whatever the
	// router does about it, it holds the same prefix afterwards.
	if !t.Run("a move to a router that kept the binding keeps the prefix", func(t *testing.T) {
		moveConcentrator(t, firstConcentrator, netip.Prefix{})
		requirePPPoE(t)
		requireDerived(t, first)
	}) {
		return
	}

	if !t.Run("a redial to the same concentrator asks for nothing", func(t *testing.T) {
		setLifetimes(t, "long")
		before := dhcpv6Requests(t)
		moveConcentrator(t, firstConcentrator, netip.Prefix{})
		requirePPPoE(t)
		time.Sleep(quietPeriod)
		if after := dhcpv6Requests(t); after != before {
			t.Fatalf("the router sent %d DHCPv6 messages after redialling the same concentrator, "+
				"which it should leave alone", after-before)
		}
		requireDerived(t, first)
	}) {
		return
	}

	if !t.Run("a session from another concentrator brings the prefix it delegates", func(t *testing.T) {
		moveConcentrator(t, secondConcentrator, second)
		session := sessionLink(t)
		requireDerived(t, second)

		// Asking for the delegation again works on the interface the session runs over,
		// and must not take the session with it.
		if now := sessionLink(t); now != session {
			t.Fatalf("the PPPoE session was dialled again while the router asked for its delegation: "+
				"the provider's end was link %d and is now link %d", session, now)
		}
		requirePPPoE(t)
	}) {
		return
	}

	t.Run("and back again", func(t *testing.T) {
		setLifetimes(t, "short")
		moveConcentrator(t, firstConcentrator, first)
		requirePPPoE(t)
		requireDerived(t, first)
	})
}

// moveConcentrator moves the session to the access concentrator with the given MAC
// address. Given a prefix, the DHCPv6 server behind it holds no binding for the router
// and delegates that prefix.
func moveConcentrator(tb testing.TB, mac string, prefix netip.Prefix) {
	tb.Helper()
	args := []string{"concentrator", mac}
	if prefix.IsValid() {
		args = append(args, prefix.String())
	}
	if out, err := topoCommand(args...); err != nil {
		tb.Fatalf("could not move the session to %s: %v\n%s", mac, err, out)
	}
}

// setLifetimes makes the DHCPv6 server hand out short or long lifetimes. Moving to the
// long ones returns once the router holds one.
func setLifetimes(tb testing.TB, which string) {
	tb.Helper()
	if out, err := topoCommand("lifetimes", which); err != nil {
		tb.Fatalf("could not make the server hand out %s lifetimes: %v\n%s", which, err, out)
	}
}

// requirePPPoE waits until traffic leaves over PPPoE again.
//
// An answer from another source is a reason to keep waiting, not a failure. Right after a
// redial the policy routing table for PPPoE is empty until the link is up, and what it
// would have carried falls through to the main table for a moment. That is not what these
// checks are about; the session coming back is.
func requirePPPoE(tb testing.TB) {
	tb.Helper()
	eventuallyPeer(tb, "outbound connectivity over PPPoE", func() (peerAddr, error) {
		got, err := tcpWhoami(tb, clientPPPoESrc, internetA)
		if err != nil {
			return got, err
		}
		if got.IP != pppoeGlobalIP {
			return got, fmt.Errorf("traffic from %s did not go over PPPoE: the source seen from outside was %s, expected %s",
				clientPPPoESrc, got.IP, pppoeGlobalIP)
		}
		return got, nil
	})
}

// dhcpv6Requests is how many messages have reached the provider's DHCPv6 server.
func dhcpv6Requests(tb testing.TB) int {
	tb.Helper()
	out, _, err := nsExec(tb, nsWAN, cmdTimeout, "", "nft", "list", "counter", "ip6", "dhcpv6", "requests")
	if err != nil {
		tb.Fatalf("could not read the DHCPv6 server's counter: %v", err)
	}
	n, err := parseCounterPackets(out)
	if err != nil {
		tb.Fatal(err)
	}
	return n
}

// sessionLink is the index of the link the provider's end of the PPPoE session is on.
// Every session gets a link of its own, and an index is never handed out twice, so a
// session dialled again shows up as a different index.
func sessionLink(tb testing.TB) int {
	tb.Helper()
	var index int
	eventually(tb, readyTimeout, "the provider's end of the PPPoE session", func() error {
		out, _, err := nsExec(tb, nsWAN, cmdTimeout, "", "ip", "-o", "-4", "route", "show", pppoeGlobalIP)
		if err != nil {
			return err
		}
		fields := strings.Fields(out)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "dev" {
				out, _, err := nsExec(tb, nsWAN, cmdTimeout, "", "cat", "/sys/class/net/"+fields[i+1]+"/ifindex")
				if err != nil {
					return err
				}
				index, err = strconv.Atoi(strings.TrimSpace(out))
				return err
			}
		}
		return fmt.Errorf("no session: %q", strings.TrimSpace(out))
	})
	return index
}
