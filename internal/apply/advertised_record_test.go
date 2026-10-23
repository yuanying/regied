package apply

import (
	"bytes"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/ddns"
)

// downstreamRecordsFixture is a host behind a router that keeps the names of itself and
// of a container behind its bridge: its own LAN address, taken from the advertisement,
// and an address cut from the delegation that advertisement is a subnet of (ADR 0024).
const downstreamRecordsFixture = downstreamFixture + `    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /etc/regied/secrets/cloudflare-example-com
        records:
          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan}
          - names: [web.example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::151"}
`

// downstreamRecordsHost is a host behind a router whose LAN took the given addresses from
// the advertisement, with a provider that records what it is asked.
func downstreamRecordsHost(t *testing.T, held ...config.AdvertisedAddress) (*Engine, fakeAdvertisements, *fakeDNS) {
	t.Helper()
	_, files, _, host := planFixture(t)
	files.put("/etc/regied/secrets/cloudflare-example-com", "super-secret-token\n", 0o600)
	advertisements := fakeAdvertisements{"eth0": held}
	host.Advertisements = advertisements
	provider := newFakeDNS()
	host.DNS = provider
	return New(host, Options{}), advertisements, provider
}

func TestPlanWritesTheRecordsOfADownstreamHost(t *testing.T) {
	engine, _, provider := downstreamRecordsHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	plan := mustPlan(t, engine, load(t, downstreamRecordsFixture))

	assertLines(t, recordsOf(plan), []string{
		`write host.example.com AAAA 2001:db8:0:101::151`,
		`write web.example.com AAAA 2001:db8:0:102::151`,
	})
	if len(plan.Waiting) != 0 {
		t.Errorf("nothing is waited for: %v", plan.Waiting)
	}
	if len(provider.calls) != 0 {
		t.Errorf("the provider was asked during planning: %v", provider.calls)
	}
	follows := map[string]string{}
	for _, change := range plan.Records {
		follows[change.Record.Name] = change.Follows
	}
	if got := follows["host.example.com"]; got != "the router advertisement on Interface/lan" {
		t.Errorf("the record taken from the advertisement says what it follows: %q", got)
	}
	if got := follows["web.example.com"]; got != "the delegation on Interface/lan" {
		t.Errorf("the record cut from the delegation says what it follows: %q", got)
	}

	// The dry run shows each address beside where it came from (ADR 0024).
	want := []string{
		"DNSRecordSet/example-com: spec.records[0] 2001:db8:0:101::151, taken from the router advertisement on Interface/lan",
		"DNSRecordSet/example-com: spec.records[1] 2001:db8:0:102::151, in 2001:db8:0:100::/56 delegated on Interface/lan",
	}
	var out bytes.Buffer
	Report(&out, plan)
	for _, line := range want {
		if !slices.Contains(plan.Delegations, line) {
			t.Errorf("the plan does not show %q:\n  %s", line, strings.Join(plan.Delegations, "\n  "))
		}
		if !strings.Contains(out.String(), line) {
			t.Errorf("the dry run does not show %q:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), "super-secret-token") {
		t.Error("the dry run printed the token")
	}
}

// Before anything has been taken from the advertisement there is no address to hold:
// both records are left as they are, and the turn names the interface it waits on
// (ADR 0024).
func TestTheRecordsOfADownstreamHostWaitForTheAdvertisement(t *testing.T) {
	engine, _, provider := downstreamRecordsHost(t)
	cfg := load(t, downstreamRecordsFixture)
	plan := mustPlan(t, engine, cfg)

	if len(plan.Records) != 0 {
		t.Errorf("a record with no address to hold is not a record to write: %v", recordsOf(plan))
	}
	for _, name := range []string{"host.example.com", "web.example.com"} {
		found := false
		for _, line := range plan.Waiting {
			if strings.Contains(line, "DNSRecordSet/example-com") && strings.Contains(line, "Interface/lan") &&
				strings.Contains(line, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("no waiting line names the set, Interface/lan and %s: %v", name, plan.Waiting)
		}
	}
	if stateOf(plan, nil) != StateWaiting {
		t.Errorf("a turn that left a record out is waiting, got %s", stateOf(plan, nil))
	}

	mustApply(t, engine, cfg)
	if len(provider.calls) != 0 {
		t.Errorf("nothing is written before the advertisement: %v", provider.calls)
	}
}

// Two addresses preferred equally long cannot be told apart. The record holding the
// advertised address waits rather than picking one.
func TestTheAdvertisedAddressRecordWaitsWhenTheAdvertisementIsAmbiguous(t *testing.T) {
	engine, _, _ := downstreamRecordsHost(t,
		advertisedOn("2001:db8:0:101::151", 1000),
		advertisedOn("2001:db8:1:201::151", 1000))
	plan := mustPlan(t, engine, load(t, downstreamRecordsFixture))

	if len(plan.Records) != 0 {
		t.Errorf("an ambiguous advertisement names no address: %v", recordsOf(plan))
	}
	found := false
	for _, line := range plan.Waiting {
		if strings.Contains(line, "host.example.com") && strings.Contains(line, "2001:db8:0:101::151") &&
			strings.Contains(line, "2001:db8:1:201::151") {
			found = true
		}
	}
	if !found {
		t.Errorf("the waiting line names both addresses: %v", plan.Waiting)
	}
}

// A prefix change on the router reaches the downstream host as a new advertisement, and
// both records move to it at once: the host's own address and the container's
// (ADR 0023, ADR 0024).
func TestTheRecordsOfADownstreamHostFollowAPrefixChange(t *testing.T) {
	engine, advertisements, provider := downstreamRecordsHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	cfg := load(t, downstreamRecordsFixture)
	mustApply(t, engine, cfg)
	assertLines(t, provider.calls, []string{
		"zone example.com", "lookup host.example.com", "update host.example.com",
		"lookup web.example.com", "update web.example.com",
	})

	// The old prefix is deprecated and the new one preferred, as networkd holds both for
	// a while after the router starts advertising the new one.
	advertisements["eth0"] = []config.AdvertisedAddress{
		advertisedOn("2001:db8:0:101::151", 0),
		advertisedOn("2001:db8:1:201::151", 2000),
	}
	plan := mustPlan(t, engine, cfg)
	assertLines(t, recordsOf(plan), []string{
		`write host.example.com AAAA 2001:db8:1:201::151`,
		`write web.example.com AAAA 2001:db8:1:202::151`,
	})
	reasons := map[string]string{}
	for _, change := range plan.Records {
		reasons[change.Record.Name] = change.Reason
	}
	if got := reasons["host.example.com"]; got != "the address moved from 2001:db8:0:101::151" {
		t.Errorf("host.example.com: %q", got)
	}
	if got := reasons["web.example.com"]; got != "the address moved from 2001:db8:0:102::151" {
		t.Errorf("web.example.com: %q", got)
	}

	provider.calls = nil
	provider.written = nil
	result := mustApply(t, engine, cfg)
	assertLines(t, provider.calls, []string{"update host.example.com", "update web.example.com"})
	written := map[string]string{}
	for _, record := range provider.written {
		if record.Type != "AAAA" {
			t.Errorf("%s was written as %s", record.Name, record.Type)
		}
		written[record.Name] = record.Content
	}
	if written["host.example.com"] != "2001:db8:1:201::151" || written["web.example.com"] != "2001:db8:1:202::151" {
		t.Errorf("written: %v", written)
	}
	if result.State != StateConverged {
		t.Errorf("state: got %s, want converged", result.State)
	}
}

// The advertisement is read for an interface a record takes its address from even when
// the interface declares no advertised delegation: the address is all the record needs.
func TestTheAdvertisedAddressIsReadWithoutAnAdvertisedDelegation(t *testing.T) {
	engine, _, _ := downstreamRecordsHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	plan := mustPlan(t, engine, load(t, `  resources:
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /etc/regied/secrets/cloudflare-example-com
        records:
          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan}
`))
	assertLines(t, recordsOf(plan), []string{`write host.example.com AAAA 2001:db8:0:101::151`})
}

// A rendering reads no advertisement. The record holding the advertised address says what
// it waits for and invents nothing; the one cut from the delegation resolves when the
// rendering is given the delegation.
func TestRenderShowsTheRecordsOfADownstreamHost(t *testing.T) {
	engine, _, _ := downstreamRecordsHost(t)
	cfg := load(t, downstreamRecordsFixture)
	runtime := &Runtime{}
	runtime.Networkd.DelegatedPrefixes = config.Delegations{"lan": {netip.MustParsePrefix("2001:db8:0:100::/56")}}

	plan, err := engine.Render(cfg, runtime)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	contents := map[string]string{}
	reasons := map[string]string{}
	for _, change := range plan.Records {
		if change.Write {
			t.Errorf("a rendering writes nothing: %+v", change)
		}
		contents[change.Record.Name] = change.Record.Content
		reasons[change.Record.Name] = change.Reason
	}
	if contents["host.example.com"] != "" || !strings.Contains(reasons["host.example.com"], "Interface/lan") {
		t.Errorf("host.example.com is shown with no address, waiting on Interface/lan: %q, %q",
			contents["host.example.com"], reasons["host.example.com"])
	}
	if contents["web.example.com"] != "2001:db8:0:102::151" {
		t.Errorf("web.example.com is cut from the delegation given: %q", contents["web.example.com"])
	}
}

// The same path for a host behind a router, through the real Cloudflare client: the
// first turn creates its own record and its container's, and a turn after the router
// starts advertising another prefix rewrites both in place (ADR 0024).
func TestAPrefixChangeRewritesTheRecordsOfADownstreamHostAtTheProvider(t *testing.T) {
	engine, advertisements, _ := downstreamRecordsHost(t, advertisedOn("2001:db8:0:101::151", 1000))
	api := newFakeCloudflareAPI("example.com")
	engine.host.DNS = ddns.NewCloudflare(&http.Client{Transport: api})
	engine = New(engine.host, Options{})
	cfg := load(t, downstreamRecordsFixture)

	if result := mustApply(t, engine, cfg); result.State != StateConverged {
		t.Fatalf("state: got %s, want converged; failing: %v", result.State, result.Plan.Failing)
	}
	assertLines(t, api.holds(), []string{
		"host.example.com AAAA 2001:db8:0:101::151",
		"web.example.com AAAA 2001:db8:0:102::151",
	})

	advertisements["eth0"] = []config.AdvertisedAddress{
		advertisedOn("2001:db8:0:101::151", 0),
		advertisedOn("2001:db8:1:201::151", 2000),
	}
	api.methods = nil
	if result := mustApply(t, engine, cfg); result.State != StateConverged {
		t.Fatalf("state: got %s, want converged; failing: %v", result.State, result.Plan.Failing)
	}
	assertLines(t, api.holds(), []string{
		"host.example.com AAAA 2001:db8:1:201::151",
		"web.example.com AAAA 2001:db8:1:202::151",
	})
	assertLines(t, api.methods, []string{http.MethodPatch, http.MethodPatch})
}
