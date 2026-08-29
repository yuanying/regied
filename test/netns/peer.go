// peerAddr lives outside the build tag. Reading a response is pure logic that needs
// neither a network namespace nor privileges, and this is exactly where an environment
// difference once broke things, so it belongs somewhere a unit test can pin it down.

package netns

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// peerAddr is the peer the whoami service saw: the shape traffic has after NAT.
type peerAddr struct {
	IP   string
	Port int
}

func (p peerAddr) String() string { return net.JoinHostPort(p.IP, strconv.Itoa(p.Port)) }

// parsePeer reads the response of the whoami service.
//
// How many pieces the response arrives in varies by environment. socat 1.8 appends an
// empty datagram to signal EOF, the receiving side answers that one too, and two
// identical observations arrive side by side. Code written on the assumption that
// exactly one line arrives at a time fails for reasons that have nothing to do with the
// router's behaviour.
//
// Identical observations standing side by side are treated as one. If they disagree,
// there is no way to tell which to believe, so this fails. Silently picking one of them
// could pass while the NAT mapping was misread.
func parsePeer(out string) (peerAddr, error) {
	var (
		first peerAddr
		found bool
	)
	for _, field := range strings.Fields(out) {
		p, err := parseOnePeer(field)
		if err != nil {
			return peerAddr{}, err
		}
		if !found {
			first, found = p, true
			continue
		}
		if p != first {
			return peerAddr{}, fmt.Errorf("the whoami responses disagree (%q)", strings.TrimSpace(out))
		}
	}
	if !found {
		return peerAddr{}, fmt.Errorf("no response from the whoami service")
	}
	return first, nil
}

func parseOnePeer(text string) (peerAddr, error) {
	host, port, err := net.SplitHostPort(text)
	if err != nil {
		return peerAddr{}, fmt.Errorf("cannot read the whoami response (%q): %w", text, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return peerAddr{}, fmt.Errorf("cannot read the port in the whoami response (%q): %w", text, err)
	}
	return peerAddr{IP: host, Port: n}, nil
}
