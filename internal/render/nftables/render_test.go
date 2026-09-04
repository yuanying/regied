package nftables_test

import (
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/nftables"
)

// doc wraps a resources block in the document every test shares. The block is written at
// the indentation it has inside spec.resources.
func doc(resources string) []byte {
	return []byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata:
  name: test
spec:
  global:
    ipForwarding: true
  resources:
` + resources)
}

// The links every test that names a zone or an uplink needs.
const links = `    - kind: Interface
      metadata: {name: wan}
      spec: {ifname: eth0}
    - kind: Interface
      metadata: {name: lan}
      spec: {ifname: br-lan}
    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan
        userIDFile: /secrets/pppoe-user-id
        passwordFile: /secrets/pppoe-password
`

// The two zones the policies below are written between.
const zones = `    - kind: FirewallZone
      metadata: {name: wan}
      spec: {linkRefs: [wan, pppoe0]}
    - kind: FirewallZone
      metadata: {name: lan}
      spec: {linkRefs: [lan]}
`

// anySecret accepts every path. None of these tests is about secrets.
type anySecret struct{}

func (anySecret) CheckSecretFile(string) error { return nil }

// render is the whole pipeline a test needs: parse, validate, render.
func render(t *testing.T, resources string) *nftables.Ruleset {
	t.Helper()
	document, err := config.Parse(doc(resources))
	if err != nil {
		t.Fatalf("the test document does not parse:\n%v", err)
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(anySecret{}))
	if err != nil {
		t.Fatalf("the test document does not validate:\n%v", err)
	}
	ruleset, err := nftables.Render(cfg)
	if err != nil {
		t.Fatalf("rendering failed: %v", err)
	}
	return ruleset
}

// rulesOf is the rules of one chain as text, or a fatal if the chain is not there.
func rulesOf(t *testing.T, ruleset *nftables.Ruleset, chain string) []string {
	t.Helper()
	for _, c := range ruleset.Chains {
		if c.Name != chain {
			continue
		}
		var out []string
		for _, rule := range c.Rules {
			if rule.Text != "" {
				out = append(out, rule.Text)
			}
		}
		return out
	}
	t.Fatalf("no chain %q; the ruleset has %s", chain, strings.Join(chainNames(ruleset), ", "))
	return nil
}

func chainNames(ruleset *nftables.Ruleset) []string {
	names := make([]string, 0, len(ruleset.Chains))
	for _, c := range ruleset.Chains {
		names = append(names, c.Name)
	}
	return names
}

func hasChain(ruleset *nftables.Ruleset, chain string) bool {
	for _, c := range ruleset.Chains {
		if c.Name == chain {
			return true
		}
	}
	return false
}

// elementsOf is one set's elements as they are written, or a fatal if the set is missing.
func elementsOf(t *testing.T, ruleset *nftables.Ruleset, set string) []string {
	t.Helper()
	for _, s := range ruleset.Sets {
		if s.Name == set {
			return s.Elements
		}
	}
	t.Fatalf("no set %q", set)
	return nil
}

// typeOf is one set's element type, or a fatal if the set is missing.
func typeOf(t *testing.T, ruleset *nftables.Ruleset, set string) string {
	t.Helper()
	for _, s := range ruleset.Sets {
		if s.Name == set {
			return s.Type
		}
	}
	t.Fatalf("no set %q", set)
	return ""
}

func assertRules(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d:\n  got:\n    %s\n  want:\n    %s",
			len(got), len(want), strings.Join(got, "\n    "), strings.Join(want, "\n    "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d:\n  got:  %s\n  want: %s", i, got[i], want[i])
		}
	}
}

func assertEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n  got:  %v\n  want: %v", what, got, want)
	}
}

// A zone is a set of interface names, and a policy is a chain dispatched to from the
// hook chain by that set. A pair with no policy is dropped, which is what the base
// chains' policy says.
func TestZonesAndPolicies(t *testing.T) {
	ruleset := render(t, links+zones+`    - kind: FirewallPolicy
      metadata: {name: lan-to-wan}
      spec:
        from: lan
        to: wan
        defaultAction: accept
    - kind: FirewallPolicy
      metadata: {name: wan-to-self}
      spec:
        from: wan
        to: self
        defaultAction: drop
        rules:
          - name: ssh
            action: accept
            protocol: tcp
            destinationPorts: [22]
`)

	// A zone is a set of kernel interface names. A PPPoE link is named after the
	// resource, so the firewall sees a stable name across redials.
	assertEqual(t, "zone_wan", elementsOf(t, ruleset, "zone_wan"), []string{`"eth0"`, `"pppoe0"`})
	assertEqual(t, "zone_lan", elementsOf(t, ruleset, "zone_lan"), []string{`"br-lan"`})

	assertRules(t, rulesOf(t, ruleset, "input"), []string{
		`iif "lo" counter accept`,
		`iifname @zone_wan jump policy_wan-to-self`,
	})
	assertRules(t, rulesOf(t, ruleset, "forward"), []string{
		`iifname @zone_lan oifname @zone_wan jump policy_lan-to-wan`,
	})
	assertRules(t, rulesOf(t, ruleset, "policy_wan-to-self"), []string{
		`ct state established,related counter accept`,
		`ct state invalid counter drop`,
		`meta l4proto tcp tcp dport 22 counter accept comment "ssh"`,
		`log prefix "regied wan-to-self default " counter drop comment "wan-to-self default"`,
	})
	// logDefault defaults to false when the default action is accept.
	assertRules(t, rulesOf(t, ruleset, "policy_lan-to-wan"), []string{
		`ct state established,related counter accept`,
		`ct state invalid counter drop`,
		`counter accept comment "lan-to-wan default"`,
	})

	for _, chain := range []string{"input", "forward"} {
		for _, c := range ruleset.Chains {
			if c.Name == chain && (c.Base == nil || c.Base.Policy != "drop") {
				t.Errorf("chain %s is not default drop: %+v", chain, c.Base)
			}
		}
	}
}

// A host that declared no firewall does not get one. Owning only what was declared is
// what keeps regied usable on a host that is not the router (ADR 0009).
func TestNoPolicyMeansNoFilterChains(t *testing.T) {
	ruleset := render(t, links+`    - kind: SourceNAT
      metadata: {name: masquerade}
      spec:
        egressRef: pppoe0
`)

	for _, chain := range []string{"input", "forward"} {
		if hasChain(ruleset, chain) {
			t.Errorf("a configuration with no FirewallPolicy got a %s chain", chain)
		}
	}
	if !hasChain(ruleset, "postrouting_nat") {
		t.Error("the SourceNAT was not rendered")
	}
}

// stateful: false leaves the two rules out. It is on by default because writing them by
// hand in every policy is how one gets forgotten.
func TestStatefulOff(t *testing.T) {
	ruleset := render(t, links+zones+`    - kind: FirewallPolicy
      metadata: {name: lan-to-wan}
      spec:
        from: lan
        to: wan
        defaultAction: drop
        logDefault: false
        stateful: false
`)

	assertRules(t, rulesOf(t, ruleset, "policy_lan-to-wan"), []string{
		`counter drop comment "lan-to-wan default"`,
	})
}

// An IPAddressSet becomes a named set. One holding prefixes is an interval set.
func TestAddressSets(t *testing.T) {
	ruleset := render(t, `    - kind: IPAddressSet
      metadata: {name: servers}
      spec:
        family: ipv4
        addresses: [192.0.2.10, 192.0.2.5]
    - kind: IPAddressSet
      metadata: {name: published}
      spec:
        family: ipv6
        addresses: [2001:db8::1]
        networks: [2001:db8:1::/64]
`)

	assertEqual(t, "addrset_servers", elementsOf(t, ruleset, "addrset_servers"),
		[]string{"192.0.2.5", "192.0.2.10"})
	assertEqual(t, "addrset_published", elementsOf(t, ruleset, "addrset_published"),
		[]string{"2001:db8::1", "2001:db8:1::/64"})

	for _, set := range ruleset.Sets {
		switch set.Name {
		case "addrset_servers":
			if set.Type != "ipv4_addr" || len(set.Flags) != 0 {
				t.Errorf("a set of addresses is not an interval set: %+v", set)
			}
		case "addrset_published":
			if set.Type != "ipv6_addr" || strings.Join(set.Flags, ",") != "interval" {
				t.Errorf("a set holding a prefix is an interval set: %+v", set)
			}
		}
	}
}

// A rule may name an address set instead of writing the addresses out, and a rule that
// names one of each is the two together rather than one of them.
func TestRuleAddressSetReferences(t *testing.T) {
	ruleset := render(t, links+zones+`    - kind: IPAddressSet
      metadata: {name: published}
      spec:
        family: ipv6
        addresses: [2001:db8::20]
    - kind: FirewallPolicy
      metadata: {name: wan-to-lan}
      spec:
        from: wan
        to: lan
        defaultAction: drop
        stateful: false
        rules:
          - name: published-web
            action: accept
            family: ipv6
            protocol: tcp
            destinationAddressSetRefs: [published]
            destinationPorts: [80, 443]
          - name: from-either
            action: accept
            family: ipv6
            sourceCIDRs: [2001:db8:2::/64]
            sourceAddressSetRefs: [published]
`)

	assertRules(t, rulesOf(t, ruleset, "policy_wan-to-lan"), []string{
		`meta nfproto ipv6 meta l4proto tcp ip6 daddr @addrset_published tcp dport { 80, 443 } counter accept comment "published-web"`,
		`meta nfproto ipv6 ip6 saddr 2001:db8:2::/64 counter accept comment "from-either"`,
		`meta nfproto ipv6 ip6 saddr @addrset_published counter accept comment "from-either"`,
		`log prefix "regied wan-to-lan default " counter drop comment "wan-to-lan default"`,
	})
}

// A rule with no family covers both, and one with a family covers that one.
func TestRuleFamilies(t *testing.T) {
	ruleset := render(t, links+zones+`    - kind: FirewallPolicy
      metadata: {name: wan-to-self}
      spec:
        from: wan
        to: self
        defaultAction: drop
        stateful: false
        rules:
          - name: icmp
            action: accept
            family: ipv4
            protocol: icmp
          - name: icmpv6
            action: accept
            family: ipv6
            protocol: icmpv6
          - name: dslite-inbound
            action: accept
            family: ipv6
            protocol: ipip
          - name: both
            action: accept
            protocol: udp
            destinationPorts: [546]
          - name: rejected
            action: reject
            family: ipv4
            sourceCIDRs: [192.0.2.0/24]
            log: true
`)

	assertRules(t, rulesOf(t, ruleset, "policy_wan-to-self"), []string{
		`meta nfproto ipv4 meta l4proto icmp counter accept comment "icmp"`,
		`meta nfproto ipv6 meta l4proto ipv6-icmp counter accept comment "icmpv6"`,
		`meta nfproto ipv6 meta l4proto 4 counter accept comment "dslite-inbound"`,
		`meta l4proto udp udp dport 546 counter accept comment "both"`,
		`meta nfproto ipv4 ip saddr 192.0.2.0/24 log prefix "regied wan-to-self rejected " counter reject comment "rejected"`,
		`log prefix "regied wan-to-self default " counter drop comment "wan-to-self default"`,
	})
}

// masquerade takes the address from the outgoing link as the packet leaves, so nothing
// about the uplink's address is written down.
func TestSourceNAT(t *testing.T) {
	ruleset := render(t, links+`    - kind: SourceNAT
      metadata: {name: masquerade-pppoe}
      spec:
        type: masquerade
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
        excludeDestinations: [192.168.20.0/24]
    - kind: SourceNAT
      metadata: {name: everything}
      spec:
        egressRef: pppoe0
`)

	assertRules(t, rulesOf(t, ruleset, "postrouting_nat"), []string{
		`oifname "pppoe0" masquerade comment "SourceNAT/everything"`,
		`oifname "pppoe0" ip saddr 192.168.10.0/24 ip daddr != 192.168.20.0/24 masquerade comment "SourceNAT/masquerade-pppoe"`,
	})
}

// A port forward is a destination translation, the hairpin pair that lets a host inside
// reach the service through the uplink's address, and the opening that lets the
// translated traffic through.
func TestPortForward(t *testing.T) {
	resources := links + zones + `    - kind: FirewallPolicy
      metadata: {name: wan-to-lan}
      spec:
        from: wan
        to: lan
        defaultAction: drop
        logDefault: false
        stateful: false
    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        target:
          address: 192.168.10.20
          port: 8443
    - kind: PortForward
      metadata: {name: mosh}
      spec:
        egressRef: pppoe0
        protocol: udp
        portRange: 60000-60010
        hairpin: false
        openFirewall: false
        target:
          address: 192.168.10.30
`
	ruleset := render(t, resources)

	assertRules(t, rulesOf(t, ruleset, "prerouting_nat"), []string{
		`iifname "pppoe0" meta nfproto ipv4 tcp dport 443 dnat ip to 192.168.10.20:8443 comment "PortForward/https"`,
		`iifname != "pppoe0" meta nfproto ipv4 ip daddr @uplink4_pppoe0 tcp dport 443 dnat ip to 192.168.10.20:8443 comment "PortForward/https"`,
		`iifname "pppoe0" meta nfproto ipv4 udp dport 60000-60010 dnat ip to 192.168.10.30:60000-60010 comment "PortForward/mosh"`,
	})
	assertRules(t, rulesOf(t, ruleset, "postrouting_nat"), []string{
		`iifname != "pppoe0" ct status dnat meta nfproto ipv4 ip daddr 192.168.10.20 tcp dport 8443 masquerade comment "PortForward/https"`,
	})
	assertRules(t, rulesOf(t, ruleset, "forward"), []string{
		`ct status dnat meta nfproto ipv4 ip daddr 192.168.10.20 tcp dport 8443 counter accept comment "PortForward/https"`,
		`ct status dnat meta nfproto ipv4 ip saddr 192.168.10.20 tcp sport 8443 counter accept comment "PortForward/https"`,
		`iifname @zone_wan oifname @zone_lan jump policy_wan-to-lan`,
	})
}

// The hairpin rule matches on the uplink's set, so it is written whether or not the line
// is up and it says nothing about what the line is holding. The set is empty here, which
// is exactly what a hairpin rule should match while the line is down (ADR 0015).
func TestHairpinMatchesTheUplinkSet(t *testing.T) {
	ruleset := render(t, links+`    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        target: {address: 192.168.10.20}
`)

	assertRules(t, rulesOf(t, ruleset, "prerouting_nat"), []string{
		`iifname "pppoe0" meta nfproto ipv4 tcp dport 443 dnat ip to 192.168.10.20:443 comment "PortForward/https"`,
		`iifname != "pppoe0" meta nfproto ipv4 ip daddr @uplink4_pppoe0 tcp dport 443 dnat ip to 192.168.10.20:443 comment "PortForward/https"`,
	})
	assertEqual(t, "uplink4_pppoe0", elementsOf(t, ruleset, "uplink4_pppoe0"), nil)

	// Nothing that only a running host knows is anywhere in the text.
	if strings.Contains(ruleset.String(), "203.0.113") {
		t.Errorf("the ruleset carries an uplink address:\n%s", ruleset.String())
	}
}

// Every uplink an egressRef may name gets a set per family, whether or not anything
// hairpins through it. The name follows from the link's name alone, because that is all
// pppd hands its hook (ADR 0015).
func TestEveryUplinkHasASetPerFamily(t *testing.T) {
	ruleset := render(t, links+`    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: wan}
        aftrHost: aftr.example.net
`)

	for _, set := range []string{"uplink4_pppoe0", "uplink6_pppoe0", "uplink4_dslite", "uplink6_dslite"} {
		assertEqual(t, set, elementsOf(t, ruleset, set), nil)
	}
	if got := typeOf(t, ruleset, "uplink4_pppoe0"); got != "ipv4_addr" {
		t.Errorf("uplink4_pppoe0 holds %s, want ipv4_addr", got)
	}
	if got := typeOf(t, ruleset, "uplink6_pppoe0"); got != "ipv6_addr" {
		t.Errorf("uplink6_pppoe0 holds %s, want ipv6_addr", got)
	}
	// The name every writer of these sets has to agree on is derived, not guessed.
	if got := nftables.UplinkSetName("pppoe0", v1alpha1.FamilyIPv4); got != "uplink4_pppoe0" {
		t.Errorf("UplinkSetName says %q", got)
	}
	if got := nftables.UplinkSetName("pppoe0", v1alpha1.FamilyIPv6); got != "uplink6_pppoe0" {
		t.Errorf("UplinkSetName says %q", got)
	}
}

// The matching half of policy routing: a source range becomes a mark. The chain sits
// after nat prerouting, which is what makes the hairpin case fall out of the exclusion.
func TestEgressRoutePolicyMarks(t *testing.T) {
	ruleset := render(t, links+`    - kind: EgressRoutePolicy
      metadata: {name: rest-via-dslite}
      spec:
        family: ipv4
        priority: 20
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
        excludeDestinations: [192.168.10.0/24, 172.16.0.0/16]
    - kind: EgressRoutePolicy
      metadata: {name: upper-half-via-pppoe}
      spec:
        family: ipv4
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.128-192.168.10.255]
        excludeDestinations: [192.168.10.0/24]
`)

	// In priority order, not document order, and with the numbers internal/config
	// derived rather than any this package chose.
	assertRules(t, rulesOf(t, ruleset, "prerouting_mark"), []string{
		`ip saddr 192.168.10.128-192.168.10.255 ip daddr != 192.168.10.0/24 meta mark set 0x100 return comment "EgressRoutePolicy/upper-half-via-pppoe"`,
		`ip saddr 192.168.10.0/24 ip daddr != { 172.16.0.0/16, 192.168.10.0/24 } meta mark set 0x101 return comment "EgressRoutePolicy/rest-via-dslite"`,
	})

	for _, c := range ruleset.Chains {
		if c.Name != "prerouting_mark" {
			continue
		}
		// After nat prerouting, so that a hairpinned packet is already addressed to the
		// host inside by the time the exclusion is considered.
		if c.Base.Hook != "prerouting" || c.Base.Priority != "filter" {
			t.Errorf("the mark chain is not after nat prerouting: %+v", c.Base)
		}
	}
}

// An IPv6 policy matches on the IPv6 header, and a source address set is an alternative
// to writing the ranges out.
func TestEgressRoutePolicyIPv6AndAddressSets(t *testing.T) {
	ruleset := render(t, links+`    - kind: IPAddressSet
      metadata: {name: upper-half}
      spec:
        family: ipv6
        networks: [2001:db8:0:1::/64]
    - kind: EgressRoutePolicy
      metadata: {name: v6-via-pppoe}
      spec:
        family: ipv6
        priority: 10
        egressRef: pppoe0
        sourceAddressSetRefs: [upper-half]
`)

	assertRules(t, rulesOf(t, ruleset, "prerouting_mark"), []string{
		`ip6 saddr @addrset_upper-half meta mark set 0x100 return comment "EgressRoutePolicy/v6-via-pppoe"`,
	})
}

// mssClamp: auto clamps on every path whose MTU is lower, rather than on one named
// interface type. off leaves the chain out; a number sets a fixed size.
func TestMSSClamp(t *testing.T) {
	for _, testCase := range []struct {
		global string
		want   []string
	}{
		{"auto", []string{`tcp flags syn / syn,rst tcp option maxseg size set rt mtu`}},
		{"1414", []string{`tcp flags syn / syn,rst tcp option maxseg size set 1414`}},
	} {
		document, err := config.Parse([]byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata: {name: test}
spec:
  global:
    mssClamp: ` + testCase.global + `
  resources: []
`))
		if err != nil {
			t.Fatalf("mssClamp %s does not parse: %v", testCase.global, err)
		}
		cfg, err := config.Validate(document)
		if err != nil {
			t.Fatalf("mssClamp %s does not validate: %v", testCase.global, err)
		}
		ruleset, err := nftables.Render(cfg)
		if err != nil {
			t.Fatalf("mssClamp %s does not render: %v", testCase.global, err)
		}
		assertRules(t, rulesOf(t, ruleset, "forward_mss"), testCase.want)
	}

	document, err := config.Parse([]byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata: {name: test}
spec:
  global: {mssClamp: "off"}
  resources: []
`))
	if err != nil {
		t.Fatalf("mssClamp off does not parse: %v", err)
	}
	cfg, err := config.Validate(document)
	if err != nil {
		t.Fatalf("mssClamp off does not validate: %v", err)
	}
	ruleset, err := nftables.Render(cfg)
	if err != nil {
		t.Fatalf("mssClamp off does not render: %v", err)
	}
	if hasChain(ruleset, "forward_mss") {
		t.Error(`mssClamp: off still clamped`)
	}
}

// regied rebuilds its own table and never flushes the ruleset (ADR 0009).
func TestOwnsOneTable(t *testing.T) {
	ruleset := render(t, links+zones)
	text := ruleset.String()

	if strings.Contains(text, "flush ruleset") {
		t.Error("the ruleset is flushed")
	}
	for _, want := range []string{
		"table inet regied\ndelete table inet regied\n",
		"table inet regied {\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the output does not replace its own table in one transaction; missing %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "\ntable "); got != 2 {
		t.Errorf("got %d table statements, want 2 (the placeholder and the new one):\n%s", got, text)
	}
}

// The same configuration renders the same ruleset, whatever order the document lists its
// resources in. The apply engine's idempotence rests on this.
func TestOrderIsStable(t *testing.T) {
	first := `    - kind: FirewallZone
      metadata: {name: wan}
      spec: {linkRefs: [wan, pppoe0]}
    - kind: FirewallZone
      metadata: {name: lan}
      spec: {linkRefs: [lan]}
    - kind: FirewallPolicy
      metadata: {name: wan-to-self}
      spec: {from: wan, to: self, defaultAction: drop}
    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec: {from: lan, to: self, defaultAction: accept}
    - kind: SourceNAT
      metadata: {name: b-masquerade}
      spec: {egressRef: pppoe0}
    - kind: SourceNAT
      metadata: {name: a-masquerade}
      spec: {egressRef: pppoe0, sourceRanges: [192.168.10.0/24]}
`
	reversed := `    - kind: SourceNAT
      metadata: {name: a-masquerade}
      spec: {egressRef: pppoe0, sourceRanges: [192.168.10.0/24]}
    - kind: SourceNAT
      metadata: {name: b-masquerade}
      spec: {egressRef: pppoe0}
    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec: {from: lan, to: self, defaultAction: accept}
    - kind: FirewallPolicy
      metadata: {name: wan-to-self}
      spec: {from: wan, to: self, defaultAction: drop}
    - kind: FirewallZone
      metadata: {name: lan}
      spec: {linkRefs: [lan]}
    - kind: FirewallZone
      metadata: {name: wan}
      spec: {linkRefs: [wan, pppoe0]}
`
	one := render(t, links+first).String()
	two := render(t, links+reversed).String()
	if one != two {
		t.Errorf("reordering the document changed the ruleset:\n%s\n---\n%s", one, two)
	}
	if again := render(t, links+first).String(); again != one {
		t.Error("rendering the same configuration twice gave two rulesets")
	}
}

// A name that cannot be an nftables identifier is refused rather than written out as
// something nft would read as another rule.
func TestUnusableName(t *testing.T) {
	document, err := config.Parse(doc(links + `    - kind: FirewallZone
      metadata: {name: "wan lan"}
      spec: {linkRefs: [wan]}
`))
	if err != nil {
		t.Fatalf("the test document does not parse: %v", err)
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(anySecret{}))
	if err != nil {
		t.Fatalf("the test document does not validate: %v", err)
	}
	if _, err := nftables.Render(cfg); err == nil {
		t.Error("a zone whose name has a space in it rendered without complaint")
	}
}
