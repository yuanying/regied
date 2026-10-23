package apply

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yuanying/regied/internal/ddns"
)

// aaaaFixture is a router that keeps two names at the address of a host behind it, cut
// from the delegation on wan (ADR 0024).
const aaaaFixture = `  resources:
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
    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /etc/regied/secrets/cloudflare-example-com
        records:
          - names: [example.com, "*.example.com"]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
`

// aaaaHost is a router holding the given delegation on eth0, or none when it is empty,
// with a provider that records what it is asked.
func aaaaHost(t *testing.T, prefix string) (*Engine, fakeDelegations, *fakeDNS) {
	t.Helper()
	_, files, _, host := planFixture(t)
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	files.put("/etc/regied/secrets/cloudflare-example-com", "super-secret-token\n", 0o600)
	// The writer is built from the provider the host has, so the provider goes in first.
	provider := newFakeDNS()
	host.DNS = provider
	engine := New(host, Options{})
	delegations := host.Delegations.(fakeDelegations)
	if prefix != "" {
		delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix(prefix)}
	}
	return engine, delegations, provider
}

func TestPlanWritesAAAARecordsCutFromTheDelegation(t *testing.T) {
	engine, _, provider := aaaaHost(t, "2001:db8:1000::/56")
	plan := mustPlan(t, engine, load(t, aaaaFixture))

	assertLines(t, recordsOf(plan), []string{
		`write *.example.com AAAA 2001:db8:1000:1::200`,
		`write example.com AAAA 2001:db8:1000:1::200`,
	})
	if len(plan.Waiting) != 0 {
		t.Errorf("nothing is waited for: %v", plan.Waiting)
	}
	if len(provider.calls) != 0 {
		t.Errorf("the provider was asked during planning: %v", provider.calls)
	}
	for _, change := range plan.Records {
		if change.Follows != "the delegation on Interface/wan" {
			t.Errorf("the record says what it follows: %q", change.Follows)
		}
	}

	// The dry run shows the address beside the delegation it was cut from (ADR 0024).
	want := "DNSRecordSet/example-com: spec.records[0] 2001:db8:1000:1::200, in 2001:db8:1000::/56 delegated on Interface/wan"
	if !slices.Contains(plan.Delegations, want) {
		t.Errorf("the plan does not show the record's address with its delegation:\n  %s", strings.Join(plan.Delegations, "\n  "))
	}
	var out bytes.Buffer
	Report(&out, plan)
	if !strings.Contains(out.String(), want) {
		t.Errorf("the dry run does not show the record's address with its delegation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "super-secret-token") {
		t.Error("the dry run printed the token")
	}
}

// With no delegation there is no address to hold, and an empty record is not a smaller
// version of the one declared: the records are left as they are, and the turn says what
// it waits for (ADR 0021, ADR 0024).
func TestAAAARecordsAreLeftAloneWhileNoDelegationIsHeld(t *testing.T) {
	engine, _, provider := aaaaHost(t, "")
	plan := mustPlan(t, engine, load(t, aaaaFixture))

	if len(plan.Records) != 0 {
		t.Errorf("a record with no address to hold is not a record to write: %v", recordsOf(plan))
	}
	found := false
	for _, line := range plan.Waiting {
		if strings.Contains(line, "DNSRecordSet/example-com") && strings.Contains(line, "Interface/wan") &&
			strings.Contains(line, "example.com") {
			found = true
		}
	}
	if !found {
		t.Errorf("the waiting line names the set, the interface and the names: %v", plan.Waiting)
	}
	if stateOf(plan, nil) != StateWaiting {
		t.Errorf("a turn that left a record out is waiting, got %s", stateOf(plan, nil))
	}

	mustApply(t, engine, load(t, aaaaFixture))
	if len(provider.calls) != 0 {
		t.Errorf("nothing is written without a delegation: %v", provider.calls)
	}
}

// A delegation that goes away after the records were written leaves them pointing at
// the prefix that was last held. A record pointing at the previous prefix for a minute
// is better than no record (ADR 0024).
func TestAAAARecordsKeepTheLastAddressWhenTheDelegationGoes(t *testing.T) {
	engine, delegations, provider := aaaaHost(t, "2001:db8:1000::/56")
	cfg := load(t, aaaaFixture)
	mustApply(t, engine, cfg)

	delete(delegations, "eth0")
	provider.calls = nil
	result := mustApply(t, engine, cfg)
	if len(provider.calls) != 0 {
		t.Errorf("a record was touched while there was no delegation: %v", provider.calls)
	}
	if result.State != StateWaiting {
		t.Errorf("state: got %s, want waiting", result.State)
	}
}

// A prefix change rewrites every AAAA record at once, with the address cut from the new
// delegation, and the plan says the address moved.
func TestAAAARecordsFollowAPrefixChange(t *testing.T) {
	engine, delegations, provider := aaaaHost(t, "2001:db8:1000::/56")
	cfg := load(t, aaaaFixture)
	mustApply(t, engine, cfg)

	delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:2000::/56")}
	plan := mustPlan(t, engine, cfg)
	assertLines(t, recordsOf(plan), []string{
		`write *.example.com AAAA 2001:db8:2000:1::200`,
		`write example.com AAAA 2001:db8:2000:1::200`,
	})
	for _, change := range plan.Records {
		if change.Reason != "the address moved from 2001:db8:1000:1::200" {
			t.Errorf("an address that moved: %q", change.Reason)
		}
	}

	provider.calls = nil
	provider.written = nil
	result := mustApply(t, engine, cfg)
	assertLines(t, provider.calls, []string{"update *.example.com", "update example.com"})
	for _, record := range provider.written {
		if record.Type != "AAAA" || record.Content != "2001:db8:2000:1::200" {
			t.Errorf("%s was written as %s %s", record.Name, record.Type, record.Content)
		}
	}
	if result.State != StateConverged {
		t.Errorf("state: got %s, want converged", result.State)
	}
}

// `regied render` with a delegation given resolves the address it would write; without
// one it says what the record follows and invents nothing.
func TestRenderShowsAnAAAARecordWithTheDelegationItWasGiven(t *testing.T) {
	engine, _, provider := aaaaHost(t, "")
	cfg := load(t, aaaaFixture)

	plan, err := engine.Render(cfg, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(plan.Records) != 2 {
		t.Fatalf("both records are shown: %v", recordsOf(plan))
	}
	for _, change := range plan.Records {
		if change.Write || change.Record.Content != "" {
			t.Errorf("no delegation was given, so there is no address: %+v", change)
		}
		if change.Follows != "the delegation on Interface/wan" {
			t.Errorf("the rendering says what the record follows: %+v", change)
		}
	}

	runtime := &Runtime{}
	runtime.Networkd.DelegatedPrefixes = map[string][]netip.Prefix{"wan": {netip.MustParsePrefix("2001:db8:1000::/56")}}
	plan, err = engine.Render(cfg, runtime)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, change := range plan.Records {
		if change.Write || change.Record.Content != "2001:db8:1000:1::200" {
			t.Errorf("the address is resolved from the delegation given: %+v", change)
		}
	}
	want := "DNSRecordSet/example-com: spec.records[0] 2001:db8:1000:1::200, in 2001:db8:1000::/56 delegated on Interface/wan"
	if !slices.Contains(plan.Delegations, want) {
		t.Errorf("the rendering does not show the record's address with its delegation:\n  %s", strings.Join(plan.Delegations, "\n  "))
	}
	var out bytes.Buffer
	Report(&out, plan)
	if !strings.Contains(out.String(), "example.com AAAA = 2001:db8:1000:1::200 in example.com follows the delegation on Interface/wan") {
		t.Errorf("the rendering does not show the address it resolved:\n%s", out.String())
	}
	if len(provider.calls) != 0 {
		t.Errorf("a rendering contacts nothing: %v", provider.calls)
	}
}

// fakeCloudflareAPI is Cloudflare's v4 API for one zone, held in memory: enough of it to
// look a record up, create one and patch one, as the real client asks for them. It is
// served without a socket, through the client's transport, so that the requests and
// answers are the client's own and `make test` still needs nothing but the toolchain.
type fakeCloudflareAPI struct {
	mu      sync.Mutex
	zone    string
	records map[string]map[string]any // by record ID
	next    int
	methods []string
}

func newFakeCloudflareAPI(zone string) *fakeCloudflareAPI {
	return &fakeCloudflareAPI{zone: zone, records: make(map[string]map[string]any)}
}

func (api *fakeCloudflareAPI) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func (api *fakeCloudflareAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.methods = append(api.methods, r.Method)

	answer := func(result any) {
		body, _ := json.Marshal(map[string]any{"success": true, "errors": []any{}, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	if r.Header.Get("Authorization") != "Bearer super-secret-token" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case r.Method == http.MethodGet && path == "/zones":
		if r.URL.Query().Get("name") != api.zone {
			answer([]any{})
			return
		}
		answer([]any{map[string]any{"id": "zone-1", "name": api.zone}})
	case r.Method == http.MethodGet && path == "/zones/zone-1/dns_records":
		var found []any
		for id, record := range api.records {
			if record["name"] == r.URL.Query().Get("name") && record["type"] == r.URL.Query().Get("type") {
				found = append(found, map[string]any{"id": id})
			}
		}
		answer(found)
	case r.Method == http.MethodPost && path == "/zones/zone-1/dns_records":
		var record map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &record)
		api.next++
		id := fmt.Sprintf("record-%d", api.next)
		api.records[id] = record
		answer(map[string]any{"id": id})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/zones/zone-1/dns_records/"):
		id := strings.TrimPrefix(path, "/zones/zone-1/dns_records/")
		record, ok := api.records[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`))
			return
		}
		var patch map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &patch)
		for key, value := range patch {
			record[key] = value
		}
		answer(map[string]any{"id": id})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"No route for that URI"}]}`))
	}
}

// holds is the zone as name and type to content, in a stable order.
func (api *fakeCloudflareAPI) holds() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []string
	for _, record := range api.records {
		out = append(out, fmt.Sprintf("%s %s %s", record["name"], record["type"], record["content"]))
	}
	slices.Sort(out)
	return out
}

// The whole path a prefix change takes, through the real Cloudflare client to an API
// that keeps what it is sent: the first turn creates the records at the address cut from
// the delegation, a turn after the delegation changes rewrites them in place, and a turn
// after that asks the provider nothing.
func TestAPrefixChangeRewritesTheAAAARecordsAtTheProvider(t *testing.T) {
	engine, delegations, _ := aaaaHost(t, "2001:db8:1000::/56")
	api := newFakeCloudflareAPI("example.com")
	engine.host.DNS = ddns.NewCloudflare(&http.Client{Transport: api})
	engine = New(engine.host, Options{})
	cfg := load(t, aaaaFixture)

	if result := mustApply(t, engine, cfg); result.State != StateConverged {
		t.Fatalf("state: got %s, want converged; failing: %v", result.State, result.Plan.Failing)
	}
	assertLines(t, api.holds(), []string{
		"*.example.com AAAA 2001:db8:1000:1::200",
		"example.com AAAA 2001:db8:1000:1::200",
	})

	delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:2000::/56")}
	api.methods = nil
	if result := mustApply(t, engine, cfg); result.State != StateConverged {
		t.Fatalf("state: got %s, want converged; failing: %v", result.State, result.Plan.Failing)
	}
	assertLines(t, api.holds(), []string{
		"*.example.com AAAA 2001:db8:2000:1::200",
		"example.com AAAA 2001:db8:2000:1::200",
	})
	// The records were found on the first turn, so the second only patches them.
	assertLines(t, api.methods, []string{http.MethodPatch, http.MethodPatch})

	api.methods = nil
	mustApply(t, engine, cfg)
	if len(api.methods) != 0 {
		t.Errorf("the provider was asked again with nothing moved: %v", api.methods)
	}
}
