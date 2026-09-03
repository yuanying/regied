package dnsmasq_test

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/dnsmasq"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// config/example.yaml is the worked example docs/spec/ refers to. Its rendering is the
// baseline the apply engine and the netns tests measure against, so it is pinned.
func TestExample(t *testing.T) {
	out := dnsmasq.Render(loadExample(t))

	if want := "/etc/regied/dnsmasq/dnsmasq.conf"; out.Config.Path != want {
		t.Errorf("configuration at %s, want %s", out.Config.Path, want)
	}
	if out.Config.Mode != 0o644 {
		t.Errorf("configuration mode %v, want 0644", out.Config.Mode)
	}
	assertGolden(t, "example.conf", out.Config.Content)
}

// Every DHCPServer and DNSForwarder renders into one configuration, so the links are the
// union of what they name. The reserved name loopback is the host itself.
func TestListeningLinks(t *testing.T) {
	content := render(t, `
    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan, addresses: [192.168.10.1/24]}
    - kind: Interface
      metadata: {name: guest}
      spec: {ifname: br-guest, addresses: [192.168.20.1/24]}
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
    - kind: DNSForwarder
      metadata: {name: lan}
      spec:
        listenOn: [lan, guest, loopback]
        upstreams: [192.0.2.53]
`)
	// bind-dynamic and nothing else: dnsmasq must not answer on the uplink.
	assertHasLines(t, content, "bind-dynamic", "interface=br-lan", "interface=br-guest", "interface=lo")
	if got := strings.Count(content, "interface=br-lan\n"); got != 1 {
		t.Errorf("br-lan named by both kinds appears %d times, want once", got)
	}
	assertLacksLines(t, content, "interface=eth0")
}

func TestPoolAndLeaseTime(t *testing.T) {
	// The lease time defaults to a day, and the pool carries the subnet's netmask.
	content := render(t, dhcpServer("        subnet: 192.168.10.0/24\n"))
	assertHasLines(t, content, "dhcp-range=set:lan,192.168.10.64,192.168.10.127,255.255.255.0,24h")

	for _, tc := range []struct{ written, want string }{
		{"12h", "12h"},
		{"90m", "90m"},
		{"45s", "45s"},
		{"1h30m", "90m"},
	} {
		content := render(t, dhcpServer("        subnet: 192.168.10.0/24\n        leaseTime: "+tc.written+"\n"))
		assertHasLines(t, content, "dhcp-range=set:lan,192.168.10.64,192.168.10.127,255.255.255.0,"+tc.want)
	}

	// A subnet that is not a /24 still gets the right mask.
	content = render(t, dhcpServer("        subnet: 192.168.10.0/23\n"))
	assertHasLines(t, content, "dhcp-range=set:lan,192.168.10.64,192.168.10.127,255.255.254.0,24h")
}

// Both default to the interface's own address, which is what the schema promises.
func TestGatewayAndResolvers(t *testing.T) {
	content := render(t, dhcpServer("        subnet: 192.168.10.0/24\n"))
	assertHasLines(t, content,
		"dhcp-option=tag:lan,option:router,192.168.10.1",
		"dhcp-option=tag:lan,option:dns-server,192.168.10.1")

	content = render(t, dhcpServer(`        subnet: 192.168.10.0/24
        gateway: 192.168.10.254
        dnsServers: [192.168.10.53, 192.0.2.53]
`))
	assertHasLines(t, content,
		"dhcp-option=tag:lan,option:router,192.168.10.254",
		"dhcp-option=tag:lan,option:dns-server,192.168.10.53,192.0.2.53")

	// An interface whose only address is derived has nothing to fall back on, and
	// dnsmasq's own default is the same rule, so nothing is written.
	content = render(t, `
    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6: {prefixDelegation: {prefixLength: 56, duidFile: /secrets/duid}}
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [{fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::1"}}]
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
`)
	if strings.Contains(content, "option:router") || strings.Contains(content, "option:dns-server") {
		t.Errorf("an interface with no literal IPv4 address has no address to hand out:\n%s", content)
	}
}

func TestStaticMappingsAndDomain(t *testing.T) {
	content := render(t, dhcpServer(`        subnet: 192.168.10.0/24
        domain: home.example
        staticMappings:
          - {name: host-a, macAddress: 00:00:5e:00:53:01, address: 192.168.10.20}
          - {name: host-b, macAddress: 00:00:5e:00:53:02, address: 192.168.10.30}
`))
	assertHasLines(t, content,
		"dhcp-host=00:00:5e:00:53:01,192.168.10.20,host-a",
		"dhcp-host=00:00:5e:00:53:02,192.168.10.30,host-b",
		// Scoped to the subnet, so a second segment can have a domain of its own.
		"domain=home.example,192.168.10.0/24")
}

// The stateless half of IPv6: answer what a client asks for after the advertisement told
// it to. The advertisement itself belongs to the interface, so dnsmasq must not send one.
func TestIPv6IsStatelessAndSendsNoAdvertisement(t *testing.T) {
	content := render(t, `
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
        ipv6: {advertise: {mode: slaac, otherInformation: true}}
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
        ipv6:
          mode: stateless
          informationRefreshTime: 2h
          dnsServers: [2001:db8:0:1::1, 2001:db8:ffff::53]
`)
	assertHasLines(t, content,
		"dhcp-range=set:lan-v6,::,constructor:br-lan,static",
		"dhcp-option=tag:lan-v6,option6:information-refresh-time,2h",
		// dnsmasq wants a literal IPv6 address in brackets here.
		"dhcp-option=tag:lan-v6,option6:dns-server,[2001:db8:0:1::1],[2001:db8:ffff::53]")

	for _, unwanted := range []string{"enable-ra", "ra-stateless", "ra-only", "ra-names", "slaac"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("%q would make dnsmasq a second source of router advertisements:\n%s", unwanted, content)
		}
	}

	// No ipv6 block, no DHCPv6 at all.
	content = render(t, dhcpServer("        subnet: 192.168.10.0/24\n"))
	if strings.Contains(content, "constructor:") {
		t.Errorf("DHCPv6 was enabled without an ipv6 block:\n%s", content)
	}
}

func TestForwardingConditionalAndStaticHosts(t *testing.T) {
	content := render(t, `
    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan, addresses: [192.168.10.1/24]}
    - kind: DNSForwarder
      metadata: {name: lan}
      spec:
        listenOn: [lan]
        cacheSize: 1000
        upstreams: [192.0.2.53, 2001:db8:ffff::53]
        conditional:
          - {domain: cluster.example, servers: [172.16.0.53, 172.16.0.54]}
        staticHosts:
          - {name: service.example.com, address: 192.168.10.20}
          - {name: service.example.com, address: 2001:db8:0:1::20}
`)
	assertHasLines(t, content,
		"cache-size=1000",
		"server=192.0.2.53",
		"server=2001:db8:ffff::53",
		// One line per server: dnsmasq takes one address per server directive.
		"server=/cluster.example/172.16.0.53",
		"server=/cluster.example/172.16.0.54",
		// host-record answers exactly this name, where address=/name/ would also
		// answer for everything below it.
		"host-record=service.example.com,192.168.10.20",
		"host-record=service.example.com,2001:db8:0:1::20")

	// The upstreams are declared, so the host's own resolver is never consulted: on this
	// host it is this process.
	assertHasLines(t, content, "no-resolv", "no-poll", "no-hosts")
}

// A host that hands out addresses but resolves nothing still needs dnsmasq, and it must
// not start answering DNS on the segment because it happens to be listening there.
func TestWithoutAForwarderDNSIsOff(t *testing.T) {
	content := render(t, dhcpServer("        subnet: 192.168.10.0/24\n"))
	assertHasLines(t, content, "port=0")
	assertLacksLines(t, content, "no-resolv")
}

// A PPPoE session is a link a forwarder may listen on, and its link carries the
// resource's name rather than the interface it runs over.
func TestListeningOnAnUplink(t *testing.T) {
	content := render(t, `
    - kind: Interface
      metadata: {name: wan}
      spec: {ifname: eth0}
    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec: {interfaceRef: wan, userIDFile: /secrets/id, passwordFile: /secrets/pw}
    - kind: DNSForwarder
      metadata: {name: everywhere}
      spec:
        listenOn: [pppoe0]
        upstreams: [192.0.2.53]
`)
	assertHasLines(t, content, "interface=pppoe0")
}

func TestRootAndStateDirectory(t *testing.T) {
	out := dnsmasq.Render(loadExample(t),
		dnsmasq.WithRoot("/run/regied-test"), dnsmasq.WithStateDirectory("/run/regied-state"))
	if want := "/run/regied-test/dnsmasq/dnsmasq.conf"; out.Config.Path != want {
		t.Errorf("configuration at %s, want %s", out.Config.Path, want)
	}
	assertHasLines(t, out.Config.Content, "dhcp-leasefile=/run/regied-state/dnsmasq.leases")
}

// Nothing regied emits for diagnosis may carry a credential (ADR 0003), and dnsmasq's
// configuration is printed by --dry-run in full.
func TestNothingSecretIsRendered(t *testing.T) {
	out := dnsmasq.Render(loadExample(t))
	if out.Config.Secret {
		t.Error("the dnsmasq configuration holds no credential and must not be marked secret")
	}
	for _, path := range []string{"/etc/regied/secrets", "pppoe-password", "pppoe-user-id"} {
		if strings.Contains(out.Config.Content, path) {
			t.Errorf("the dnsmasq configuration mentions %q", path)
		}
	}
}

// A host that hands out no addresses and resolves nothing has no reason to run dnsmasq.
func TestNothingToServe(t *testing.T) {
	out := dnsmasq.Render(load(t, `
    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan, addresses: [192.168.10.1/24]}
`))
	if out.Config.Path != "" || out.Config.Content != "" || len(out.Files()) != 0 {
		t.Errorf("a configuration with neither kind rendered %+v", out.Config)
	}
}

// --- helpers ---------------------------------------------------------------------

// dhcpServer is one server on one interface, with the spec lines a case wants.
func dhcpServer(spec string) string {
	return `
    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan, addresses: [192.168.10.1/24]}
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        pool: {start: 192.168.10.64, end: 192.168.10.127}
` + spec
}

func render(t *testing.T, resources string) string {
	t.Helper()
	return dnsmasq.Render(load(t, resources)).Config.Content
}

func load(t *testing.T, resources string) *config.Config {
	t.Helper()
	document := []byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata:
  name: test
spec:
  resources:
` + resources)
	parsed, err := config.Parse(document)
	if err != nil {
		t.Fatalf("parsing the test configuration: %v", err)
	}
	cfg, err := config.Validate(parsed, config.WithSecretFiles(anySecret{}))
	if err != nil {
		t.Fatalf("validating the test configuration: %v", err)
	}
	return cfg
}

func loadExample(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../../config/example.yaml", config.WithSecretFiles(anySecret{}))
	if err != nil {
		t.Fatalf("config/example.yaml does not validate:\n%v", err)
	}
	return cfg
}

// anySecret stands in for the filesystem. Nothing dnsmasq renders comes from a secret.
type anySecret struct{}

func (anySecret) CheckSecretFile(string) error { return nil }

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), fs.FileMode(0o644)); err != nil {
			t.Fatalf("rewriting %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s does not match. Run `go test ./internal/render/dnsmasq -update` to see the whole of it.\n--- want ---\n%s\n--- got ---\n%s",
			path, want, got)
	}
}

func assertHasLines(t *testing.T, content string, want ...string) {
	t.Helper()
	lines := strings.Split(content, "\n")
	for _, line := range want {
		if !slices.Contains(lines, line) {
			t.Errorf("no line %q in:\n%s", line, content)
		}
	}
}

func assertLacksLines(t *testing.T, content string, unwanted ...string) {
	t.Helper()
	lines := strings.Split(content, "\n")
	for _, line := range unwanted {
		if slices.Contains(lines, line) {
			t.Errorf("unwanted line %q in:\n%s", line, content)
		}
	}
}
