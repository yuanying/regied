package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// A field that was mistyped must not be silently dropped. That is the way this kind of
// configuration breaks worst: the file is accepted, the setting is not there, and
// nothing says so until the traffic it was meant to govern goes the wrong way.
func TestParseRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		yaml []byte
		want string
	}{
		{
			name: "in a spec",
			yaml: doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        mtuu: 1500
`),
			want: `unknown field "mtuu"`,
		},
		{
			name: "in metadata",
			yaml: doc(`    - kind: Interface
      metadata: {name: lan, labels: x}
      spec:
        ifname: br-lan
`),
			want: `unknown field "labels"`,
		},
		{
			name: "in a resource",
			yaml: doc(`    - kind: Interface
      metadata: {name: lan}
      status: up
      spec:
        ifname: br-lan
`),
			want: `unknown field "status"`,
		},
		{
			name: "in a nested spec field",
			yaml: doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
            duidfile: /etc/regied/secrets/duid
`),
			want: `unknown field "duidfile"`,
		},
		{
			name: "in a list entry",
			yaml: doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        routes:
          - destination: 172.16.0.0/16
            gateway: 192.168.10.10
`),
			want: `unknown field "gateway"`,
		},
		{
			name: "in global",
			yaml: []byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata: {name: test}
spec:
  global:
    allPing: true
  resources: []
`),
			want: `unknown field "allPing"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse(tc.yaml)
			if err == nil {
				t.Fatal("parsed a document with an unknown field")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %s:\n%v", tc.want, err)
			}
		})
	}
}

// One run of the parser should report every malformed thing in the file. Fixing one
// mistake per run is how a file with six of them takes six runs.
func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, err := config.Parse(doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        mtuu: 1500
    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        addresses:
          - 192.0.2.300/24
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        leaseTime: yesterday
`))
	if err == nil {
		t.Fatal("parsed a document with three malformed values")
	}
	for _, want := range []string{`unknown field "mtuu"`, `"192.0.2.300/24"`, `"yesterday"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

// Every message carries the line it was found on.
func TestParseErrorsCarryLineNumbers(t *testing.T) {
	_, err := config.Parse(doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        mtuu: 1500
`))
	if err == nil {
		t.Fatal("parsed a document with an unknown field")
	}
	if !strings.Contains(err.Error(), "line 13") {
		t.Errorf("error does not point at line 13:\n%v", err)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	_, err := config.Parse(doc(`    - kind: BGPPeer
      metadata: {name: upstream}
      spec: {}
`))
	if err == nil || !strings.Contains(err.Error(), `unknown resource kind "BGPPeer"`) {
		t.Fatalf("want an unknown-kind error, got: %v", err)
	}
}

func TestParseRejectsWrongDocumentType(t *testing.T) {
	cases := []struct {
		name, apiVersion, kind, want string
	}{
		{"apiVersion", "example.com/v1", "NetworkConfig", "net.unstable.cloud/v1alpha1"},
		{"kind", "net.unstable.cloud/v1alpha1", "Router", "NetworkConfig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte("apiVersion: " + tc.apiVersion + "\nkind: " + tc.kind + "\nmetadata: {name: test}\nspec: {resources: []}\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %s, got: %v", tc.want, err)
			}
		})
	}
}

// One YAML document per host. A second one in the same file would otherwise be read and
// thrown away without a word.
func TestParseRejectsASecondDocument(t *testing.T) {
	first := doc(`    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan}
`)
	_, err := config.Parse(append(first, []byte("---\napiVersion: net.unstable.cloud/v1alpha1\nkind: NetworkConfig\nmetadata: {name: second}\nspec: {resources: []}\n")...))
	if err == nil || !strings.Contains(err.Error(), "one document") {
		t.Fatalf("want a second-document error, got: %v", err)
	}
}

// A key written twice means one of the two values is being thrown away.
func TestParseRejectsDuplicateKeys(t *testing.T) {
	_, err := config.Parse(doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        ifname: br-wan
`))
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("want a duplicate-key error, got: %v", err)
	}
}

func TestParseRejectsMalformedValues(t *testing.T) {
	cases := []struct {
		name, spec, want string
	}{
		{"address with a prefix length", "ifname: eth0\n        addresses: [192.0.2.1]", `"192.0.2.1"`},
		{"prefix out of range", "ifname: eth0\n        addresses: [192.0.2.1/33]", `"192.0.2.1/33"`},
		{"route destination", "ifname: eth0\n        routes: [{destination: not-a-network}]", `"not-a-network"`},
		{"next hop", "ifname: eth0\n        routes: [{destination: 172.16.0.0/16, via: 192.0.2}]", `"192.0.2"`},
		{"duration", "ifname: eth0\n        ipv6: {advertise: {mode: slaac, validLifetime: 24hours}}", `"24hours"`},
		{"router advertisement mode", "ifname: eth0\n        ipv6: {advertise: {mode: managed}}", `"managed"`},
		{"an address written as a mapping without a form", "ifname: eth0\n        addresses: [{}]", "exactly one of fromDelegatedPrefix and fromRouterAdvertisement"},
		{"an address written as a mapping with two forms", "ifname: eth0\n        addresses: [{fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1}, fromRouterAdvertisement: {token: \"::1\"}}]", "exactly one of fromDelegatedPrefix and fromRouterAdvertisement"},
		{"an unknown key in fromRouterAdvertisement", "ifname: eth0\n        addresses: [{fromRouterAdvertisement: {token: \"::1\", prefix: \"2001:db8::/64\"}}]", "prefix"},
		{"a Wake-on-LAN mode networkd does not have", "ifname: eth0\n        wakeOnLan: always", `"always"`},
		{"a Wake-on-LAN mode networkd does not have, in a list", "ifname: eth0\n        wakeOnLan: [magic, never]", `"never"`},
		{"Wake-on-LAN written as a mapping", "ifname: eth0\n        wakeOnLan: {magic: true}", "a Wake-on-LAN mode or a list of them"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse(doc("    - kind: Interface\n      metadata: {name: lan}\n      spec:\n        " + tc.spec + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %s, got: %v", tc.want, err)
			}
		})
	}
}

func TestParseRejectsMalformedValuesInOtherKinds(t *testing.T) {
	cases := []struct {
		name, resource, want string
	}{
		{
			"source range that ends before it starts",
			`    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.0.2.255-192.0.2.1]`,
			"ends before it starts",
		},
		{
			"source range written as a bare address",
			`    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.0.2.1]`,
			"neither a CIDR nor an address range",
		},
		{
			"source range mixing families",
			`    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.0.2.1-2001:db8::1]`,
			"mixes address families",
		},
		{
			"firewall action",
			`    - kind: FirewallPolicy
      metadata: {name: p}
      spec:
        from: lan
        to: wan
        defaultAction: allow`,
			`"allow"`,
		},
		{
			"protocol",
			`    - kind: FirewallPolicy
      metadata: {name: p}
      spec:
        from: lan
        to: wan
        defaultAction: drop
        rules:
          - {name: r, action: accept, protocol: sctp}`,
			`"sctp"`,
		},
		{
			"port range",
			`    - kind: FirewallPolicy
      metadata: {name: p}
      spec:
        from: lan
        to: wan
        defaultAction: drop
        rules:
          - {name: r, action: accept, destinationPorts: [70000]}`,
			`"70000"`,
		},
		{
			"MAC address",
			`    - kind: DHCPServer
      metadata: {name: d}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
        staticMappings:
          - {name: h, macAddress: 00:00:5e:00:53, address: 192.168.10.20}`,
			`"00:00:5e:00:53"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse(doc(tc.resource + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %s, got: %v", tc.want, err)
			}
		})
	}
}

// Wake-on-LAN is written as one mode or as a list of them, and leaving it out is a third
// answer: the NIC is left as it is. An empty list is not the same as leaving it out, so
// that validation can refuse it rather than read it as either.
func TestParseAcceptsWakeOnLanAsOneModeOrAList(t *testing.T) {
	cases := []struct {
		name, spec string
		want       v1alpha1.WakeOnLan
	}{
		{"left out", "ifname: eth0", nil},
		{"one mode", "ifname: eth0\n        wakeOnLan: magic", v1alpha1.WakeOnLan{v1alpha1.WakeOnLanMagic}},
		{"a list", "ifname: eth0\n        wakeOnLan: [magic, unicast]", v1alpha1.WakeOnLan{v1alpha1.WakeOnLanMagic, v1alpha1.WakeOnLanUnicast}},
		{"an empty list", "ifname: eth0\n        wakeOnLan: []", v1alpha1.WakeOnLan{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := config.Parse(doc("    - kind: Interface\n      metadata: {name: lan}\n      spec:\n        " + tc.spec + "\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := parsed.Spec.Resources[0].Spec.(*v1alpha1.InterfaceSpec).WakeOnLan
			if (got == nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
				t.Errorf("wakeOnLan is %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedGlobal(t *testing.T) {
	_, err := config.Parse([]byte("apiVersion: net.unstable.cloud/v1alpha1\nkind: NetworkConfig\nmetadata: {name: test}\nspec:\n  global: {mssClamp: sometimes}\n  resources: []\n"))
	if err == nil || !strings.Contains(err.Error(), `"sometimes"`) {
		t.Fatalf("want an error mentioning the value, got: %v", err)
	}
}

// The forms a value may be written in, all of which have to survive a round trip through
// the parser.
func TestParseAcceptsEveryWrittenForm(t *testing.T) {
	cfgDoc, err := config.Parse(doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses:
          - 192.168.10.1/24
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::1"}
          - fromRouterAdvertisement: {token: "::53"}
    - kind: PortForward
      metadata: {name: single}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        target: {address: 192.168.10.20}
    - kind: PortForward
      metadata: {name: ranged}
      spec:
        egressRef: pppoe0
        protocol: udp
        portRange: 60000-60010
        target: {address: 192.168.10.30, portRange: 60000-60010}
    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
        defaultAction: drop
        rules:
          - {name: gre, action: accept, protocol: 47, destinationPorts: [80, 60000-60010]}
    - kind: EgressRoutePolicy
      metadata: {name: policy}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges:
          - 192.168.10.0/24
          - 192.168.10.128-192.168.10.255
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	iface := cfgDoc.Spec.Resources[0].Spec.(*v1alpha1.InterfaceSpec)
	if !iface.Addresses[0].IsLiteral() || iface.Addresses[0].Literal.String() != "192.168.10.1/24" {
		t.Errorf("literal address: %+v", iface.Addresses[0])
	}
	derived := iface.Addresses[1].FromDelegatedPrefix
	if derived == nil || derived.InterfaceRef != "wan" || derived.SubnetID == nil || *derived.SubnetID != 1 || derived.Token != "::1" {
		t.Errorf("derived address: %+v", iface.Addresses[1])
	}
	advertised := iface.Addresses[2]
	if advertised.IsLiteral() || advertised.FromDelegatedPrefix != nil ||
		advertised.FromRouterAdvertisement == nil || advertised.FromRouterAdvertisement.Token != "::53" {
		t.Errorf("address from the router advertisement: %+v", advertised)
	}

	single := cfgDoc.Spec.Resources[1].Spec.(*v1alpha1.PortForwardSpec)
	if got := single.Ports().String(); got != "443" {
		t.Errorf("single port: %s", got)
	}
	if got := single.TargetPorts().String(); got != "443" {
		t.Errorf("target defaults to the same port: %s", got)
	}

	ranged := cfgDoc.Spec.Resources[2].Spec.(*v1alpha1.PortForwardSpec)
	if got := ranged.Ports().String(); got != "60000-60010" {
		t.Errorf("port range: %s", got)
	}

	rule := cfgDoc.Spec.Resources[3].Spec.(*v1alpha1.FirewallPolicySpec).Rules[0]
	if got := rule.Protocol.String(); got != "47" {
		t.Errorf("numeric protocol: %s", got)
	}
	if got := rule.DestinationPorts[0].String(); got != "80" {
		t.Errorf("single port in a rule: %s", got)
	}
	if got := rule.DestinationPorts[1].String(); got != "60000-60010" {
		t.Errorf("port range in a rule: %s", got)
	}

	policy := cfgDoc.Spec.Resources[4].Spec.(*v1alpha1.EgressRoutePolicySpec)
	if !policy.SourceRanges[0].IsPrefix() {
		t.Error("a CIDR source range should be a prefix")
	}
	if policy.SourceRanges[1].IsPrefix() || policy.SourceRanges[1].String() != "192.168.10.128-192.168.10.255" {
		t.Errorf("range source range: %+v", policy.SourceRanges[1])
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, doc("    - kind: Interface\n      metadata: {name: lan}\n      spec: {ifnamee: br-lan}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path, config.WithSecretFiles(anySecret{}))
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("want an error naming %s, got: %v", path, err)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"), config.WithSecretFiles(anySecret{}))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want a not-exist error, got: %v", err)
	}
}
