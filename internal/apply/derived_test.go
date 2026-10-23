package apply

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// derivedFixture is a router that routes a subnet of its delegation to a host behind it,
// and publishes that host and the subnet through an address set, both cut from the
// delegation on wan (ADR 0021).
const derivedFixture = `  resources:
    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
            duidFile: /etc/regied/secrets/dhcpv6-duid
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses:
          - 192.168.10.1/24
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::1"}
        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2}
            via:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::10"}
    - kind: IPAddressSet
      metadata: {name: published}
      spec:
        family: ipv6
        addresses:
          - 2001:db8:ffff::1
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
        networks:
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2}
`

const lanNetwork = "/etc/systemd/network/50-regied-lan.network"

func derivedHost(t *testing.T, prefix string) (*Engine, *fakeFiles, *fakeRunner, fakeDelegations) {
	t.Helper()
	engine, files, runner, host := planFixture(t)
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	delegations := host.Delegations.(fakeDelegations)
	if prefix != "" {
		delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix(prefix)}
	}
	return engine, files, runner, delegations
}

// intervalSetsHold makes the kernel answer that regied's table holds these sets, in the
// JSON nft 1.1 prints for an interval set: an address as a string, a network as a prefix
// object.
func intervalSetsHold(runner *fakeRunner, sets map[string][]string) {
	entries := []string{`{"metainfo":{"version":"1.1.3","release_name":"x","json_schema_version":1}}`,
		`{"table":{"family":"inet","name":"regied","handle":1}}`}
	names := make([]string, 0, len(sets))
	for name := range sets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		var elements []string
		for _, element := range sets[name] {
			if prefix, err := netip.ParsePrefix(element); err == nil {
				elements = append(elements, fmt.Sprintf(`{"prefix":{"addr":"%s","len":%d}}`, prefix.Addr(), prefix.Bits()))
				continue
			}
			elements = append(elements, `"`+element+`"`)
		}
		elem := ""
		if len(elements) > 0 {
			elem = `,"elem":[` + strings.Join(elements, ",") + `]`
		}
		entries = append(entries, fmt.Sprintf(`{"set":{"family":"inet","name":"%s","table":"regied","type":"ipv6_addr","handle":2,"flags":["interval"]%s}}`, name, elem))
	}
	runner.output[listTableCommand().String()] = `{"nftables":[` + strings.Join(entries, ",") + `]}`
}

// nft prints a network in an interval set as a prefix object. Read as the CIDR it is, it
// compares equal to the network a turn would write, so a set that already holds it is
// not written again.
func TestAPrefixElementIsReadAsItsCIDR(t *testing.T) {
	runner := newFakeRunner()
	intervalSetsHold(runner, map[string][]string{"addrset_published": {"2001:db8:1000:1::200", "2001:db8:1000:2::/64"}})
	held, err := parseSets([]byte(runner.output[listTableCommand().String()]))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2001:db8:1000:1::200", "2001:db8:1000:2::/64"}
	if !slices.Equal(held["addrset_published"], want) {
		t.Errorf("read %q, want %q", held["addrset_published"], want)
	}
}

// With the delegation read, the route goes into the .network as literals, and the set
// is written with every element, its literal one included (ADR 0021).
func TestAPlanResolvesDerivedRoutesAndSetElements(t *testing.T) {
	engine, _, _, _ := derivedHost(t, "2001:db8:1000::/56")
	plan := mustPlan(t, engine, load(t, derivedFixture))

	network := changeFor(plan, lanNetwork)
	if !strings.Contains(network.Content, "[Route]\nDestination=2001:db8:1000:2::/64\nGateway=2001:db8:1000:1::10\n") {
		t.Errorf("the route was not resolved from the delegation:\n%s", network.Content)
	}
	want := SetElements{Set: "addrset_published", Elements: []string{"2001:db8:1000:1::200", "2001:db8:1000:2::/64", "2001:db8:ffff::1"}}
	if !slices.ContainsFunc(plan.Firewall.Elements, func(s SetElements) bool { return s.String() == want.String() }) {
		t.Errorf("the plan writes %v, want it to write %v", plan.Firewall.Elements, want)
	}
	if strings.Contains(plan.Firewall.Ruleset, "2001:db8:1000") {
		t.Errorf("the delegated prefix went into the ruleset text:\n%s", plan.Firewall.Ruleset)
	}
	if len(plan.Waiting) != 0 {
		t.Errorf("a plan with the delegation waits for %q", plan.Waiting)
	}

	var out bytes.Buffer
	Report(&out, plan)
	for _, want := range []string{
		"Interface/wan: delegated 2001:db8:1000::/56",
		"Interface/lan: spec.routes[0] to 2001:db8:1000:2::/64 via 2001:db8:1000:1::10",
		"IPAddressSet/published: spec.addresses[1] 2001:db8:1000:1::200",
		"IPAddressSet/published: spec.networks[0] 2001:db8:1000:2::/64",
		"addrset_published { 2001:db8:1000:1::200, 2001:db8:1000:2::/64, 2001:db8:ffff::1 }",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry-run does not show %q:\n%s", want, out.String())
		}
	}
}

// The provider delegated another prefix. Nothing was submitted: an unattended turn
// rewrites the route, reloads networkd, and writes the set with the new elements, and the
// host is converged again (ADR 0021).
func TestAnUnattendedTurnMovesRoutesAndSetsToANewDelegation(t *testing.T) {
	engine, files, runner, delegations := derivedHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, derivedFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	intervalSetsHold(runner, map[string][]string{"addrset_published": {"2001:db8:1000:1::200", "2001:db8:1000:2::/64", "2001:db8:ffff::1"}})

	delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:2000:ab00::/56")}
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}

	network, _ := files.content(lanNetwork)
	if !strings.Contains(network, "Destination=2001:db8:2000:ab02::/64\nGateway=2001:db8:2000:ab01::10\n") {
		t.Errorf("the route was not moved to the new delegation:\n%s", network)
	}
	ran := commandsSince(runner, before)
	if !slices.Contains(ran, "networkctl reload") {
		t.Errorf("networkd was not reloaded; ran %q", ran)
	}
	seed := seedOf(runner, before)
	if !strings.Contains(seed, "flush set inet regied addrset_published\n") ||
		!strings.Contains(seed, "add element inet regied addrset_published { 2001:db8:2000:ab01::200, 2001:db8:2000:ab02::/64, 2001:db8:ffff::1 }\n") {
		t.Errorf("the set was not written with the new delegation; the seeding was:\n%s", seed)
	}
	for _, cmd := range runner.ran[before:] {
		if strings.Contains(cmd.Stdin, "delete table inet regied") {
			t.Error("the table was replaced for a change of its sets' elements")
		}
	}
	if result.State != StateConverged {
		t.Errorf("the turn says %q, want converged; failing %v, waiting %v", result.State, result.Plan.Failing, result.Plan.Waiting)
	}

	// The set now holds the new elements, and the turn after it has nothing to write.
	intervalSetsHold(runner, map[string][]string{"addrset_published": {"2001:db8:2000:ab01::200", "2001:db8:2000:ab02::/64", "2001:db8:ffff::1"}})
	before = len(runner.ran)
	result, err = engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if seed := seedOf(runner, before); seed != "" {
		t.Errorf("a turn with the same delegation wrote the sets again:\n%s", seed)
	}
	if result.State != StateConverged {
		t.Errorf("the next turn says %q, want converged; failing %v", result.State, result.Plan.Failing)
	}
}

// With the delegation gone, the derived route and the derived elements are absent: the
// subnet they name does not exist. The literal element stays, the rest of the file is
// written as declared, and the turn waits, naming the interface (ADR 0021).
func TestATurnWithoutADelegationLeavesOutOnlyTheDerivedEntries(t *testing.T) {
	engine, files, runner, delegations := derivedHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, derivedFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	intervalSetsHold(runner, map[string][]string{"addrset_published": {"2001:db8:1000:1::200", "2001:db8:1000:2::/64", "2001:db8:ffff::1"}})

	delete(delegations, "eth0")
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}
	if result.State != StateWaiting {
		t.Errorf("the turn says %q, want waiting", result.State)
	}
	waiting := strings.Join(result.Plan.Waiting, "\n")
	for _, want := range []string{
		"Interface/lan: waiting for a delegated prefix on Interface/wan; spec.routes[0] is left out",
		"IPAddressSet/published: waiting for a delegated prefix on Interface/wan; spec.addresses[1] is left out",
		"IPAddressSet/published: waiting for a delegated prefix on Interface/wan; spec.networks[0] is left out",
	} {
		if !strings.Contains(waiting, want) {
			t.Errorf("the turn does not say %q:\n%s", want, waiting)
		}
	}
	network, ok := files.content(lanNetwork)
	if !ok || strings.Contains(network, "[Route]") || !strings.Contains(network, "Address=192.168.10.1/24") {
		t.Errorf("the .network does not hold everything but the route:\n%s", network)
	}
	seed := seedOf(runner, before)
	if !strings.Contains(seed, "add element inet regied addrset_published { 2001:db8:ffff::1 }\n") {
		t.Errorf("the set was not left holding its literal element; the seeding was:\n%s", seed)
	}
}

// A rendering is given the prefix on the command line, and shows the elements a turn
// would write beside the ruleset that declares the set empty (ADR 0021).
func TestARenderingShowsTheDerivedSetElements(t *testing.T) {
	engine := New(Host{}, Options{})
	runtime := &Runtime{}
	runtime.Networkd.DelegatedPrefixes = map[string][]netip.Prefix{"wan": {netip.MustParsePrefix("2001:db8:1000::/56")}}
	runtime.Networkd.DUIDs = map[string]string{"/etc/regied/secrets/dhcpv6-duid": "00:03:00:01:00:00:5e:00:53:01"}
	plan, err := engine.Render(load(t, derivedFixture), runtime)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	Report(&out, plan)
	if !strings.Contains(out.String(), "addrset_published { 2001:db8:1000:1::200, 2001:db8:1000:2::/64, 2001:db8:ffff::1 }") {
		t.Errorf("the rendering does not show the set's elements:\n%s", out.String())
	}
}

// seedOf is what every set-writing command since the nth wrote, as one text.
func seedOf(runner *fakeRunner, n int) string {
	var b strings.Builder
	for _, cmd := range runner.ran[n:] {
		if cmd.Name == "nft" && strings.Contains(cmd.Stdin, "flush set") && !strings.Contains(cmd.Stdin, "delete table") {
			b.WriteString(cmd.Stdin)
		}
	}
	return b.String()
}
