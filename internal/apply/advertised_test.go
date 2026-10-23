package apply

import (
	"bytes"
	"context"
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

// describedAdvertisement is networkd's description of a link holding a static IPv4
// address, its link-local address, the address it took from the advertisement with its
// token, a temporary one beside it, and one networkd describes with no lifetime.
const describedAdvertisement = `{"Index":2,"Name":"eth0","Addresses":[` +
	`{"Family":2,"Address":[192,168,10,2],"PrefixLength":24,"ConfigSource":"static"},` +
	`{"Family":10,"Address":[254,128,0,0,0,0,0,0,0,0,0,0,0,0,1,81],"PrefixLength":64,"ConfigSource":"foreign"},` +
	`{"Family":10,"Address":[32,1,13,184,0,0,1,1,0,0,0,0,0,0,1,81],"PrefixLength":64,"ConfigSource":"NDisc","PreferredLifetimeUSec":7124103304,"ValidLifetimeUSec":8924103304},` +
	`{"Family":10,"Address":[32,1,13,184,0,0,1,1,170,170,187,187,204,204,221,221],"PrefixLength":64,"ConfigSource":"NDisc","PreferredLifetimeUSec":1000,"ValidLifetimeUSec":2000},` +
	`{"Family":10,"Address":[32,1,13,184,0,0,171,1,0,0,0,0,0,0,1,81],"PrefixLength":64,"ConfigSource":"NDisc"}` +
	`]}`

// What a downstream host reads is the addresses networkd configured from an
// advertisement, with how long each is preferred. Anything networkd configured from
// another source is not an advertisement's (ADR 0023).
func TestTheAdvertisedAddressesAreReadFromNetworkdsDescriptionOfTheLink(t *testing.T) {
	got, err := parseAdvertisedAddresses([]byte(describedAdvertisement))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.AdvertisedAddress{
		{Address: netip.MustParseAddr("2001:db8:0:101::151"), PreferredUntil: 7124103304},
		{Address: netip.MustParseAddr("2001:db8:0:101:aaaa:bbbb:cccc:dddd"), PreferredUntil: 1000},
		{Address: netip.MustParseAddr("2001:db8:0:ab01::151"), PreferredUntil: math.MaxUint64},
	}
	if !slices.Equal(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
	}
}

func TestALinkWithoutAddressesAdvertisesNone(t *testing.T) {
	got, err := parseAdvertisedAddresses([]byte(`{"Index":2,"Name":"eth0"}`))
	if err != nil || len(got) != 0 {
		t.Errorf("read %v, %v; want none", got, err)
	}
	if _, err := parseAdvertisedAddresses([]byte(`{"Addresses":[{"Family":10,"Address":[1,2],"ConfigSource":"NDisc"}]}`)); err == nil {
		t.Error("an address of two bytes was accepted")
	}
}

// downstreamFixture is a host behind a router, whose container bridge is cut from the
// delegation its LAN is advertised a subnet of (ADR 0023).
const downstreamFixture = `  resources:
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
        ipv6:
          advertisedDelegation: {prefixLength: 56}
    - kind: Interface
      metadata: {name: containers}
      spec:
        ifname: br-v6net
        bridge: {}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::1"}
        ipv6:
          advertise: {mode: slaac}
`

// fakeAdvertisements answers the addresses each link took from an advertisement from a
// table.
type fakeAdvertisements map[string][]config.AdvertisedAddress

func (a fakeAdvertisements) AdvertisedAddresses(_ context.Context, ifname string) ([]config.AdvertisedAddress, error) {
	return a[ifname], nil
}

func advertisedOn(address string, preferred uint64) config.AdvertisedAddress {
	return config.AdvertisedAddress{Address: netip.MustParseAddr(address), PreferredUntil: preferred}
}

func downstreamHost(t *testing.T, held ...config.AdvertisedAddress) (*Engine, *fakeFiles, *fakeRunner, fakeAdvertisements) {
	t.Helper()
	host, files, runner := testHost()
	tableAbsent(runner)
	advertisements := fakeAdvertisements{"eth0": held}
	host.Advertisements = advertisements
	return New(host, Options{}), files, runner, advertisements
}

// A turn knows the delegation on a downstream host from the address its LAN took from the
// advertisement, cut back to the declared length, and keys it by the Interface, as it
// does a router's (ADR 0023).
func TestCollectRuntimeCutsTheDelegationFromTheAdvertisement(t *testing.T) {
	cfg := load(t, downstreamFixture)
	host, _, _ := testHost()
	host.Advertisements = fakeAdvertisements{"eth0": {advertisedOn("2001:db8:0:101::151", 1000)}}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	got := rt.Networkd.DelegatedPrefixes["lan"]
	if len(got) != 1 || got[0].String() != "2001:db8:0:100::/56" {
		t.Errorf("the delegation of Interface/lan is %v, want [2001:db8:0:100::/56]", got)
	}
}

// During a change the new prefix is taken, and the choice is said.
func TestCollectRuntimeSaysWhichAdvertisedDelegationItTook(t *testing.T) {
	cfg := load(t, downstreamFixture)
	host, _, _ := testHost()
	host.Advertisements = fakeAdvertisements{"eth0": {
		advertisedOn("2001:db8:0:101::151", 1000),
		advertisedOn("2001:db8:0:ab01::151", 5000),
	}}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Networkd.DelegatedPrefixes["lan"]; len(got) != 1 || got[0].String() != "2001:db8:0:ab00::/56" {
		t.Errorf("the delegation of Interface/lan is %v, want the one preferred longest", got)
	}
	want := "Interface/lan is advertised subnets of 2 delegations; took 2001:db8:0:ab00::/56, which is preferred longest, over 2001:db8:0:100::/56"
	if !strings.Contains(strings.Join(rt.Notes, "\n"), want) {
		t.Errorf("the notes do not say %q: %q", want, rt.Notes)
	}
}

// The provider moved the router to another prefix, and the router advertises a subnet of
// it. Nothing was submitted on the downstream host: an unattended turn rewrites the
// bridge's address and advertisement and reloads networkd (ADR 0023).
func TestAnUnattendedTurnMovesTheDownstreamBridgeToTheNewPrefix(t *testing.T) {
	engine, files, runner, advertisements := downstreamHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	mustSubmit(t, engine, downstreamFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	bridge, _ := files.content("/etc/systemd/network/50-regied-containers.network")
	if !strings.Contains(bridge, "Address=2001:db8:0:102::1/64\n") || !strings.Contains(bridge, "Prefix=2001:db8:0:102::/64\n") {
		t.Fatalf("the bridge was not cut from the advertised delegation:\n%s", bridge)
	}

	// Both prefixes are advertised for a while; the new one is preferred longer.
	advertisements["eth0"] = []config.AdvertisedAddress{
		advertisedOn("2001:db8:0:101::151", 1000),
		advertisedOn("2001:db8:0:ab01::151", 5000),
	}
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}
	bridge, _ = files.content("/etc/systemd/network/50-regied-containers.network")
	if !strings.Contains(bridge, "Address=2001:db8:0:ab02::1/64\n") || !strings.Contains(bridge, "Prefix=2001:db8:0:ab02::/64\n") {
		t.Errorf("the bridge was not moved to the new delegation:\n%s", bridge)
	}
	if strings.Contains(bridge, "2001:db8:0:102::") {
		t.Errorf("the bridge still names the old delegation:\n%s", bridge)
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("networkd was not reloaded; ran %q", commandsSince(runner, before))
	}
	if result.State != StateConverged {
		t.Errorf("the turn says %q, want converged; failing %v, waiting %v", result.State, result.Plan.Failing, result.Plan.Waiting)
	}
}

// Before the router has advertised anything, the bridge is written without its derived
// address and the turn waits, naming the interface.
func TestADownstreamTurnWaitsForTheAdvertisement(t *testing.T) {
	engine, files, _, _ := downstreamHost(t)
	result := mustSubmit(t, engine, downstreamFixture, "/etc/regied/config.yaml")

	if result.State != StateWaiting {
		t.Errorf("the turn says %q, want waiting", result.State)
	}
	if !strings.Contains(strings.Join(result.Plan.Waiting, "\n"), "a delegated prefix on Interface/lan") {
		t.Errorf("the turn does not say it waits for the delegation on Interface/lan: %q", result.Plan.Waiting)
	}
	bridge, ok := files.content("/etc/systemd/network/50-regied-containers.network")
	if !ok || strings.Contains(bridge, "2001:db8:") {
		t.Errorf("the bridge is not written without its derived address:\n%s", bridge)
	}
}

// The dry-run shows the delegation the host cut from the advertisement, and says where
// it came from.
func TestAPlanShowsTheDelegationCutFromTheAdvertisement(t *testing.T) {
	engine, _, _, _ := downstreamHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	plan := mustPlan(t, engine, load(t, downstreamFixture))

	var out bytes.Buffer
	Report(&out, plan)
	for _, want := range []string{
		"Interface/lan: delegated 2001:db8:0:100::/56, cut from the prefix advertised to it",
		"Interface/containers: spec.addresses[0] 2001:db8:0:102::1/64, in 2001:db8:0:100::/56 delegated on Interface/lan",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry-run does not show %q:\n%s", want, out.String())
		}
	}
}
