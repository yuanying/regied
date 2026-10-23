package apply

import (
	"bytes"
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// What `networkctl status --json=short` prints for a link whose DHCPv6 client holds a
// delegation, trimmed to the parts that matter, as systemd 257 prints it: the prefix is
// sixteen bytes rather than a string (ADR 0021).
const describedDelegation = `{"Index":3,"Name":"wan","Kind":"veth","DHCPv6Client":{"Lease":{"Timeout1USec":2764103786,"Timeout2USec":3304103786,"LeaseTimestampUSec":1864103786},"Prefixes":[{"Prefix":[32,1,13,184,16,0,0,0,0,0,0,0,0,0,0,0],"PrefixLength":56,"PreferredLifetimeUSec":3664103786,"ValidLifetimeUSec":5464103786}],"DUID":[0,3,0,1,2,0,0,0,0,1]}}`

func TestTheDelegationIsReadFromNetworkdsDescriptionOfTheLink(t *testing.T) {
	prefixes, err := parseDelegatedPrefixes([]byte(describedDelegation))
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 1 || prefixes[0] != netip.MustParsePrefix("2001:db8:1000::/56") {
		t.Errorf("read %v, want [2001:db8:1000::/56]", prefixes)
	}
}

// A client still soliciting has no DHCPv6Client object yet, or one without prefixes.
// Neither is an error: there is no delegation, and the turn waits for one.
func TestALinkWithoutADelegationDescribesNone(t *testing.T) {
	for _, description := range []string{
		`{"Index":3,"Name":"wan"}`,
		`{"Index":3,"Name":"wan","DHCPv6Client":{"DUID":[0,3]}}`,
		`{"Index":3,"Name":"wan","DHCPv6Client":{"Prefixes":[]}}`,
	} {
		prefixes, err := parseDelegatedPrefixes([]byte(description))
		if err != nil {
			t.Errorf("%s: %v", description, err)
		}
		if len(prefixes) != 0 {
			t.Errorf("%s: read %v, want none", description, prefixes)
		}
	}
}

func TestADescriptionThatIsNotUnderstoodIsAnError(t *testing.T) {
	for _, description := range []string{
		`not json`,
		`{"DHCPv6Client":{"Prefixes":[{"Prefix":[32,1],"PrefixLength":56}]}}`,
		`{"DHCPv6Client":{"Prefixes":[{"Prefix":[32,1,13,184,16,0,0,0,0,0,0,0,0,0,0,0],"PrefixLength":129}]}}`,
	} {
		if _, err := parseDelegatedPrefixes([]byte(description)); err == nil {
			t.Errorf("%s: parsed, want an error", description)
		}
	}
}

// delegationFixture is a router whose tunnel sends from the address lan cuts from the
// delegation on wan (ADR 0022).
const delegationFixture = `  resources:
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
    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrAddress: 2001:db8:ffff::1
`

func delegationHost(t *testing.T, prefix string) (*Engine, *fakeFiles, *fakeRunner, fakeDelegations) {
	t.Helper()
	engine, files, runner, host := planFixture(t)
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	delegations := host.Delegations.(fakeDelegations)
	if prefix != "" {
		delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix(prefix)}
	}
	return engine, files, runner, delegations
}

// A turn reads the prefix each prefix-delegation client holds, keyed by the Interface
// running it, from the link that Interface names (ADR 0021).
func TestCollectRuntimeReadsTheDelegatedPrefix(t *testing.T) {
	cfg := load(t, delegationFixture)
	host, files, _ := testHost()
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	host.Delegations = fakeDelegations{"eth0": {netip.MustParsePrefix("2001:db8:1000::/56")}}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	got := rt.Networkd.DelegatedPrefixes["wan"]
	if len(got) != 1 || got[0].String() != "2001:db8:1000::/56" {
		t.Errorf("the delegation of Interface/wan is %v, want [2001:db8:1000::/56]", got)
	}
}

// networkd not answering is not a failure of the turn: there is no delegation to
// resolve against, which is a reason to wait, and the turn says why.
func TestCollectRuntimeNotesADelegationThatCannotBeRead(t *testing.T) {
	cfg := load(t, delegationFixture)
	host, files, _ := testHost()
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	host.Delegations = failingDelegations{}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("an unreadable delegation failed the turn: %v", err)
	}
	if _, ok := rt.Networkd.DelegatedPrefixes["wan"]; ok {
		t.Errorf("a delegation was recorded although none could be read")
	}
	if !strings.Contains(strings.Join(rt.Notes, "\n"), "Interface/wan") {
		t.Errorf("the notes do not say whose delegation could not be read: %q", rt.Notes)
	}
}

// The dry-run shows the prefix that was read and the value resolved from it (ADR 0021).
func TestAPlanShowsTheDelegationAndWhatWasResolvedFromIt(t *testing.T) {
	engine, _, _, _ := delegationHost(t, "2001:db8:1000::/56")
	plan := mustPlan(t, engine, load(t, delegationFixture))

	netdev := changeFor(plan, "/etc/systemd/network/50-regied-dslite.netdev")
	if !strings.Contains(netdev.Content, "Local=2001:db8:1000:1::1\n") {
		t.Errorf("the tunnel does not send from the delegated address:\n%s", netdev.Content)
	}
	var out bytes.Buffer
	Report(&out, plan)
	for _, want := range []string{
		"Interface/wan: delegated 2001:db8:1000::/56",
		"DSLiteTunnel/dslite: local address 2001:db8:1000:1::1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry-run does not show %q:\n%s", want, out.String())
		}
	}
}

// The provider moved the line and delegated another prefix. Nothing was submitted: an
// unattended turn rewrites the tunnel's netdev and reloads networkd, which changes the
// local address of the tunnel already up, and the host is converged again (ADR 0022).
func TestAnUnattendedTurnMovesTheTunnelToANewDelegation(t *testing.T) {
	engine, files, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:2000:ab00::/56")}
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}

	netdev, _ := files.content("/etc/systemd/network/50-regied-dslite.netdev")
	if !strings.Contains(netdev, "Local=2001:db8:2000:ab01::1\n") {
		t.Errorf("the tunnel was not moved to the new delegation:\n%s", netdev)
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("networkd was not reloaded; ran %q", commandsSince(runner, before))
	}
	if result.State != StateConverged {
		t.Errorf("the turn says %q, want converged; failing %v, waiting %v", result.State, result.Plan.Failing, result.Plan.Waiting)
	}

	// And the turn after it has nothing left to do.
	before = len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Error("a turn with the same delegation reloaded networkd again")
	}
}

// With the delegation gone — a lease that expired, a client soliciting again — the
// tunnel an earlier turn wrote is left exactly as it was, and the turn waits, naming the
// interface (ADR 0021, ADR 0022).
func TestATurnWithoutADelegationLeavesTheTunnelAndWaits(t *testing.T) {
	engine, files, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	written, _ := files.content("/etc/systemd/network/50-regied-dslite.netdev")

	delete(delegations, "eth0")
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}
	if result.State != StateWaiting {
		t.Errorf("the turn says %q, want waiting", result.State)
	}
	if !strings.Contains(strings.Join(result.Plan.Waiting, "\n"), "a delegated prefix on Interface/wan") {
		t.Errorf("the turn does not say it waits for the delegation on Interface/wan: %q", result.Plan.Waiting)
	}
	if got, ok := files.content("/etc/systemd/network/50-regied-dslite.netdev"); !ok || got != written {
		t.Errorf("the tunnel was not left as it was:\n%s", got)
	}
	if _, ok := files.content("/etc/systemd/network/50-regied-dslite.network"); !ok {
		t.Error("the tunnel's .network was reclaimed while it waited")
	}
}
