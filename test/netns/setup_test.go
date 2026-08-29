//go:build netns

package netns

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixed values of the topology. These are paired with hack/netns/lib.sh. Do not
// change one side without the other.
const (
	nsClient   = "rg-client"
	nsInternet = "rg-internet"

	// The three source addresses the client netns owns. They straddle the range so that
	// the policy-routing split can be told apart from outside.
	clientPPPoESrc  = "192.168.1.20"  // 192.168.1.10-99 goes over PPPoE
	clientDSLiteSrc = "192.168.1.200" // anything else goes over DS-Lite (the default)
	lanServerIP     = "192.168.1.30"  // the port forward's destination, inside the PPPoE range

	routerLANIP = "192.168.0.1"

	// The global addresses seen from outside. Which of these appears is what tells the
	// two paths apart.
	pppoeGlobalIP  = "198.51.100.2" // the router's address, handed out over PPPoE
	dsliteGlobalIP = "192.0.2.1"    // the outside address the AFTR uses for NAT44

	// The two addresses of the internet netns. Two destinations are provided so that the
	// NAT mapping can be shown not to depend on the destination.
	internetA = "203.0.113.10"
	internetB = "203.0.113.20"

	whoamiTCPPort = 8080
	whoamiUDPPort = 9999

	forwardWANPort = 8022 // from outside, forwarded to 192.168.1.30:22
	forwardLANPort = 22
	blockedWANPort = 9999 // neither forwarded nor allowed

	sshStubBanner = "sshd-stub"
)

// Timeouts. Connectivity is retried for a while, to allow for a PPPoE reconnection or
// for a tunnel coming up.
const (
	readyTimeout = 45 * time.Second
	cmdTimeout   = 20 * time.Second
)

func TestMain(m *testing.M) {
	if reason := unmetPrerequisite(); reason != "" {
		// Skipping silently when the run was meant to happen in an environment that has
		// the tools makes something that never ran look like it passed. Runs that go
		// through the container have REGIED_NETNS_REQUIRE set, so those fail instead.
		if os.Getenv("REGIED_NETNS_REQUIRE") != "" {
			fmt.Fprintf(os.Stderr, "a prerequisite of the netns tests is not met: %s\n", reason)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "skipping the netns tests: %s\n", reason)
		fmt.Fprintf(os.Stderr, "to run them in an environment that has the tools, use `make test-netns-docker`.\n")
		os.Exit(0)
	}

	if out, err := topo("up"); err != nil {
		fmt.Fprintf(os.Stderr, "could not build the topology: %v\n%s\n", err, out)
		if _, derr := topo("down"); derr != nil {
			fmt.Fprintf(os.Stderr, "the teardown failed as well: %v\n", derr)
		}
		os.Exit(1)
	}

	code := m.Run()

	if os.Getenv("REGIED_NETNS_KEEP") != "" {
		fmt.Fprintf(os.Stderr, "REGIED_NETNS_KEEP is set, so the topology is left up. "+
			"Run hack/netns/topo.sh down to remove it.\n")
	} else if out, err := topo("down"); err != nil {
		fmt.Fprintf(os.Stderr, "the teardown failed: %v\n%s\n", err, out)
	}

	os.Exit(code)
}

// unmetPrerequisite returns why a prerequisite is missing, or the empty string if all
// of them are met.
func unmetPrerequisite() string {
	if os.Geteuid() != 0 {
		return "not running as root (creating a netns needs CAP_NET_ADMIN)"
	}
	for _, bin := range []string{"ip", "nft", "pppd", "pppoe-server", "socat"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Sprintf("%s not found", bin)
		}
	}
	return ""
}

// topo calls the script that builds and tears down the pseudo WAN. Replacing the device
// under test happens inside that script, through an environment variable.
func topo(action string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(root, "hack", "netns", "topo.sh"), action)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func repoRoot() (string, error) {
	// go test runs with the package directory (test/netns) as its working directory.
	return filepath.Abs(filepath.Join("..", ".."))
}

// nsExec runs a command inside the named network namespace and returns its standard
// output, how long it took, and the result. The elapsed time is what tells "dropped"
// (a timeout) apart from "refused" (an RST came back).
func nsExec(tb testing.TB, ns string, timeout time.Duration, stdin string, args ...string) (string, time.Duration, error) {
	tb.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	full := append([]string{"netns", "exec", ns}, args...)
	cmd := exec.CommandContext(ctx, "ip", full...)
	cmd.Stdin = strings.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		err = fmt.Errorf("%s: %w (%s)", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), elapsed, err
}

// tcpWhoami picks a source address in the client netns, connects over TCP to the whoami
// service in the internet netns, and returns the source that was seen from outside.
func tcpWhoami(tb testing.TB, srcIP, dstIP string) (peerAddr, error) {
	tb.Helper()
	out, _, err := nsExec(tb, nsClient, cmdTimeout, "",
		"socat", "-t", "3", "-T", "10", "STDIO",
		fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=5", dstIP, whoamiTCPPort, srcIP))
	if err != nil {
		return peerAddr{}, err
	}
	return parsePeer(out)
}

// udpWhoami calls the UDP whoami service with the source port pinned as well. It is
// used to show that the external port does not change when the destination does, which
// is endpoint-independent mapping.
func udpWhoami(tb testing.TB, srcIP string, srcPort int, dstIP string) (peerAddr, error) {
	tb.Helper()
	out, _, err := nsExec(tb, nsClient, cmdTimeout, "probe\n",
		"socat", "-t", "3", "-T", "10", "STDIO",
		// Without shut-none, socat 1.8 appends an empty datagram to signal EOF. The
		// receiving side treats that as one more query, so two responses come back. The
		// reading side absorbs this too, but keeping the behaviour on the wire the same
		// regardless of environment is worth doing.
		fmt.Sprintf("UDP4:%s:%d,bind=%s:%d,shut-none", dstIP, whoamiUDPPort, srcIP, srcPort))
	if err != nil {
		return peerAddr{}, err
	}
	return parsePeer(out)
}

// eventuallyPeer retries until connectivity comes up, because the PPPoE session or the
// tunnel's route can take a few seconds to appear.
func eventuallyPeer(tb testing.TB, what string, probe func() (peerAddr, error)) peerAddr {
	tb.Helper()

	deadline := time.Now().Add(readyTimeout)
	var last error
	for {
		p, err := probe()
		if err == nil {
			return p
		}
		last = err
		if time.Now().After(deadline) {
			tb.Fatalf("%s was not established within %s: %v", what, readyTimeout, last)
		}
		time.Sleep(time.Second)
	}
}
